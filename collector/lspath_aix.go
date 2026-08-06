// collector/lspath_aix.go
//go:build aix
// +build aix

package collector

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/client_golang/prometheus"
)

// ---------- Flags ----------
var (
	lspathTimeout = kingpin.Flag(
		"collector.aix_lspath.timeout",
		"Timeout per lspath invocation. Keep small to avoid scrape overruns.").
		Default("3s").Duration()

	lspathPath = kingpin.Flag(
		"collector.aix_lspath.lspath-path",
		"Path to lspath (leave empty to resolve via PATH).").
		Default("lspath").String()
)

// ---------- Registration ----------
func init() {
	registerCollector("aix_lspath", defaultDisabled, NewAIXLspathCollector)
}

// ---------- Collector ----------
type aixLspathCollector struct {
	logger     *slog.Logger
	timeout    time.Duration
	lspathPath string

	// Per-path: status gauge with full label set
	descPathStatus *prometheus.Desc

	// Aggregations
	descDiskTotal          *prometheus.Desc // total paths per disk
	descDiskEnabled        *prometheus.Desc // enabled paths per disk
	descAdapterTotal       *prometheus.Desc // total paths per adapter (HBA)
	descAdapterEnabled     *prometheus.Desc // enabled paths per adapter
	descControllerTotal    *prometheus.Desc // total paths per storage controller (WWN)
	descControllerEnabled  *prometheus.Desc // enabled paths per storage controller
}

func NewAIXLspathCollector(logger *slog.Logger) (Collector, error) {
	if logger == nil {
		logger = slog.Default()
	}

	ns := namespace
	return &aixLspathCollector{
		logger:     logger,
		timeout:    *lspathTimeout,
		lspathPath: *lspathPath,

		descPathStatus: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "lspath", "status"),
			"SAN path status: 1=Enabled, 0=any other state. Labels identify the full path.",
			[]string{"disk", "adapter", "wwn", "lun", "state"}, nil,
		),

		descDiskTotal: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "lspath", "disk_total_paths"),
			"Total number of SAN paths for a disk across all adapters and controllers.",
			[]string{"disk"}, nil,
		),
		descDiskEnabled: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "lspath", "disk_enabled_paths"),
			"Number of enabled SAN paths for a disk.",
			[]string{"disk"}, nil,
		),

		descAdapterTotal: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "lspath", "adapter_total_paths"),
			"Total number of SAN paths through an HBA adapter.",
			[]string{"adapter"}, nil,
		),
		descAdapterEnabled: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "lspath", "adapter_enabled_paths"),
			"Number of enabled SAN paths through an HBA adapter.",
			[]string{"adapter"}, nil,
		),

		descControllerTotal: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "lspath", "controller_total_paths"),
			"Total number of SAN paths to a storage controller port (WWN).",
			[]string{"wwn"}, nil,
		),
		descControllerEnabled: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "lspath", "controller_enabled_paths"),
			"Number of enabled SAN paths to a storage controller port (WWN).",
			[]string{"wwn"}, nil,
		),
	}, nil
}

// ---------- Update ----------

func (c *aixLspathCollector) Update(ch chan<- prometheus.Metric) error {
	paths, err := c.collectPaths()
	if err != nil {
		c.logger.Debug("lspath collection failed", "err", err)
		return nil // non-fatal: scrape continues without lspath metrics
	}
	if len(paths) == 0 {
		return nil
	}

	sort.Slice(paths, func(i, j int) bool {
		a, b := paths[i], paths[j]
		if a.Disk != b.Disk {
			return a.Disk < b.Disk
		}
		if a.Adapter != b.Adapter {
			return a.Adapter < b.Adapter
		}
		if a.WWN != b.WWN {
			return a.WWN < b.WWN
		}
		return a.LUN < b.LUN
	})

	// -- per-path status --
	for _, p := range paths {
		val := 0.0
		if p.IsEnabled {
			val = 1.0
		}
		ch <- prometheus.MustNewConstMetric(
			c.descPathStatus, prometheus.GaugeValue, val,
			p.Disk, p.Adapter, p.WWN, p.LUN, p.State,
		)
	}

	// -- aggregation counters --
	diskTotal := make(map[string]int)
	diskEnabled := make(map[string]int)
	adapterTotal := make(map[string]int)
	adapterEnabled := make(map[string]int)
	controllerTotal := make(map[string]int)
	controllerEnabled := make(map[string]int)

	for _, p := range paths {
		diskTotal[p.Disk]++
		adapterTotal[p.Adapter]++
		if p.WWN != "" {
			controllerTotal[p.WWN]++
		}
		if p.IsEnabled {
			diskEnabled[p.Disk]++
			adapterEnabled[p.Adapter]++
			if p.WWN != "" {
				controllerEnabled[p.WWN]++
			}
		}
	}

	for disk, n := range diskTotal {
		ch <- prometheus.MustNewConstMetric(c.descDiskTotal, prometheus.GaugeValue, float64(n), disk)
		ch <- prometheus.MustNewConstMetric(c.descDiskEnabled, prometheus.GaugeValue, float64(diskEnabled[disk]), disk)
	}
	for adapter, n := range adapterTotal {
		ch <- prometheus.MustNewConstMetric(c.descAdapterTotal, prometheus.GaugeValue, float64(n), adapter)
		ch <- prometheus.MustNewConstMetric(c.descAdapterEnabled, prometheus.GaugeValue, float64(adapterEnabled[adapter]), adapter)
	}
	for wwn, n := range controllerTotal {
		ch <- prometheus.MustNewConstMetric(c.descControllerTotal, prometheus.GaugeValue, float64(n), wwn)
		ch <- prometheus.MustNewConstMetric(c.descControllerEnabled, prometheus.GaugeValue, float64(controllerEnabled[wwn]), wwn)
	}

	return nil
}

// ---------- Model ----------

type lspathRow struct {
	Disk      string // e.g. hdisk15
	Adapter   string // e.g. fscsi3  (HBA / FC adapter)
	WWN       string // e.g. 2006d039eae1b7aa  (storage controller port WWN)
	LUN       string // e.g. 0, 1000000000000  (LUN ID in hex, as reported)
	State     string // lowercase: "enabled", "failed", "missing", "defined"
	IsEnabled bool
}

// ---------- Collection: try formatted, fall back to plain ----------

func (c *aixLspathCollector) collectPaths() ([]lspathRow, error) {
	// Prefer the rich format that includes WWN and LUN.
	out, err := c.runCmd(c.lspathPath, "-F", "name:connection:parent:status")
	if err == nil {
		rows, parseErr := parseFormattedLspath(out)
		if parseErr == nil && len(rows) > 0 {
			return rows, nil
		}
		// Parsed zero rows from formatted output — could be an empty system or
		// a format error; fall through to plain lspath.
		c.logger.Debug("lspath -F returned no rows, falling back to plain lspath", "parseErr", parseErr)
	} else {
		c.logger.Debug("lspath -F failed, falling back to plain lspath", "err", err)
	}

	// Fallback: plain `lspath` — no WWN/LUN available.
	out, err = c.runCmd(c.lspathPath)
	if err != nil {
		return nil, err
	}
	return parsePlainLspath(out)
}

// ---------- Formatted parser: lspath -F "name:connection:parent:status" ----------
//
// Output line format:
//   hdisk15:2006d039eae1b7aa,0:fscsi3:Enabled
//
// Fields: name : connection : parent : status
// connection = <wwn>,<lun_hex>   (comma-separated inside the field)
// A disk can appear multiple times — once per storage-port path. All rows are kept.

func parseFormattedLspath(out []byte) ([]lspathRow, error) {
	var rows []lspathRow
	seen := make(map[string]bool)

	for _, rawLine := range strings.Split(string(out), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}

		parts := strings.Split(line, ":")
		if len(parts) < 4 {
			continue // malformed line; skip
		}

		disk := strings.TrimSpace(parts[0])
		connection := strings.TrimSpace(parts[1]) // "<wwn>,<lun>" or just "<lun>"
		adapter := strings.TrimSpace(parts[2])
		state := strings.ToLower(strings.TrimSpace(parts[3]))

		if disk == "" || adapter == "" || state == "" {
			continue
		}

		// Split connection into WWN and LUN.
		wwn, lun := splitConnection(connection)

		// Unique key: the full path is disk + adapter + wwn + lun.
		key := disk + "|" + adapter + "|" + wwn + "|" + lun
		if seen[key] {
			continue
		}
		seen[key] = true

		rows = append(rows, lspathRow{
			Disk:      disk,
			Adapter:   adapter,
			WWN:       wwn,
			LUN:       lun,
			State:     state,
			IsEnabled: state == "enabled",
		})
	}

	return rows, nil
}

// splitConnection splits a connection string of the form "<wwn>,<lun>" into
// its two parts. If there is no comma (older AIX or unusual output), the whole
// string is treated as the LUN and WWN is returned empty.
func splitConnection(connection string) (wwn, lun string) {
	idx := strings.Index(connection, ",")
	if idx < 0 {
		return "", connection
	}
	return connection[:idx], connection[idx+1:]
}

// ---------- Plain parser: lspath (no flags) ----------
//
// Output line format (whitespace-separated):
//   Enabled  hdisk0  fscsi0
//
// Fields: status  name  parent
// No WWN or LUN information available; those fields are left empty.

func parsePlainLspath(out []byte) ([]lspathRow, error) {
	var rows []lspathRow
	seen := make(map[string]bool)

	for _, rawLine := range strings.Split(string(out), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}

		// Split on any whitespace; need at least 3 fields.
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}

		state := strings.ToLower(parts[0])
		disk := parts[1]
		adapter := parts[2]

		// Plain lspath: unique key is disk + adapter (no WWN/LUN to distinguish).
		key := disk + "|" + adapter
		if seen[key] {
			continue
		}
		seen[key] = true

		rows = append(rows, lspathRow{
			Disk:      disk,
			Adapter:   adapter,
			WWN:       "",
			LUN:       "",
			State:     state,
			IsEnabled: state == "enabled",
		})
	}

	return rows, nil
}

// ---------- exec helper ----------

func (c *aixLspathCollector) runCmd(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	out, err := cmd.CombinedOutput()

	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("%s %v timed out after %s", name, args, c.timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("%s %v failed: %v (out=%s)", name, args, err, string(out))
	}
	return out, nil
}
