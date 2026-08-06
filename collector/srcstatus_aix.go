// collector/aix_srcstatus.go
//go:build aix
// +build aix

package collector

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/client_golang/prometheus"
)

// ---------- Flags ----------
var (
	srcstatusSubsystems = kingpin.Flag(
		"collector.aix_srcstatus.subsystems",
		"Comma-separated SRC subsystems to check (e.g., netcd,xntpd,ssh). 'ssh' is normalized to 'sshd'.").
		Default("netcd,xntpd,ssh,syslogd").String()

	srcstatusMode = kingpin.Flag(
		"collector.aix_srcstatus.mode",
		"Collection mode: bulk (lssrc -a once), single (lssrc -s per subsystem), or auto (bulk then fallback).").
		Default("bulk").Enum("bulk", "single", "auto")

	srcstatusTimeout = kingpin.Flag(
		"collector.aix_srcstatus.timeout",
		"Timeout per lssrc invocation. Keep small to avoid scrape overruns.").
		Default("3s").Duration()

	srcstatusPath = kingpin.Flag(
		"collector.aix_srcstatus.lssrc-path",
		"Path to lssrc (leave empty to resolve via PATH).").
		Default("lssrc").String()
)

// ---------- Registration ----------
func init() {
	registerCollector("aix_srcstatus", defaultDisabled, NewAIXSRCStatusCollector)
}

// ---------- Collector ----------
type aixSRCStatusCollector struct {
	logger    *slog.Logger
	subsys    []string
	mode      string
	timeout   time.Duration
	lssrcPath string

	// Descriptors
	descUp   *prometheus.Desc
	descPID  *prometheus.Desc
	descInfo *prometheus.Desc
}

func NewAIXSRCStatusCollector(logger *slog.Logger) (Collector, error) {
	if logger == nil {
		logger = slog.Default()
	}
	// Parse/normalize target subsystems once.
	raw := splitCSV(*srcstatusSubsystems)
	targets := make([]string, 0, len(raw))
	for _, s := range raw {
		switch strings.ToLower(s) {
		case "ssh":
			targets = append(targets, "sshd")
		default:
			targets = append(targets, s)
		}
	}
	// sort for stable label order
	sort.Strings(targets)

	ns := namespace // "node"
	return &aixSRCStatusCollector{
		logger:    logger,
		subsys:    targets,
		mode:      *srcstatusMode,
		timeout:   *srcstatusTimeout,
		lssrcPath: *srcstatusPath,
		descUp: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "src", "subsystem_up"),
			"SRC subsystem up (1=active, 0=not active/inoperative/absent).",
			[]string{"subsystem", "group"}, nil,
		),
		descPID: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "src", "subsystem_pid"),
			"PID of SRC subsystem when active, else 0.",
			[]string{"subsystem", "group"}, nil,
		),
		descInfo: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "src", "subsystem_status_info"),
			"Last observed short status as a label (always 1).",
			[]string{"subsystem", "group", "status"}, nil,
		),
	}, nil
}

func (c *aixSRCStatusCollector) Update(ch chan<- prometheus.Metric) error {
	// Bulk collection if requested.
	var bulk map[string]srcRow
	var bulkErr error
	if c.mode == "bulk" || c.mode == "auto" {
		bulk, bulkErr = c.runLssrcAll()
	}

	for _, name := range c.subsys {
		row, ok := bulk[name]
		if ok {
			emitRow(ch, c.descUp, c.descPID, c.descInfo, name, row)
			continue
		}
		if c.mode == "single" || c.mode == "auto" {
			row, err := c.runLssrcSingle(name)
			if err == nil {
				emitRow(ch, c.descUp, c.descPID, c.descInfo, name, row)
				continue
			}
			// Not found / timeout → mark as absent
			row = srcRow{Group: "unknown", PID: 0, Status: "absent"}
			emitRow(ch, c.descUp, c.descPID, c.descInfo, name, row)
			continue
		}
		// bulk only and not found → absent
		row = srcRow{Group: "unknown", PID: 0, Status: "absent"}
		emitRow(ch, c.descUp, c.descPID, c.descInfo, name, row)
	}

	// Optionally log bulk error (doesn't fail the whole collector).
	if bulkErr != nil && c.mode != "single" {
		c.logger.Debug("lssrc -a failed or partial", "err", bulkErr)
	}
	return nil
}

// ---------- Model & helpers ----------
type srcRow struct {
	Group  string
	PID    int
	Status string // "active" | "inoperative" | "unknown" | "absent"
}

func (r srcRow) Up() float64 {
	if strings.EqualFold(r.Status, "active") {
		return 1
	}
	return 0
}

func emitRow(ch chan<- prometheus.Metric, up, pid, info *prometheus.Desc, name string, row srcRow) {
	ch <- prometheus.MustNewConstMetric(up, prometheus.GaugeValue, row.Up(), name, row.Group)
	ch <- prometheus.MustNewConstMetric(pid, prometheus.GaugeValue, float64(row.PID), name, row.Group)
	ch <- prometheus.MustNewConstMetric(info, prometheus.GaugeValue, 1, name, row.Group, row.Status)
}

// ----- lssrc -a (bulk) -----
func (c *aixSRCStatusCollector) runLssrcAll() (map[string]srcRow, error) {
	out, err := c.runCmd(c.lssrcPath, "-a")
	if err != nil {
		return nil, err
	}
	// Map name -> best row (prefer 'active' if duplicates exist).
	best := map[string]srcRow{}
	sc := newWideScanner(out)
	headerSeen := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "Subsystem") {
			headerSeen = true
			continue
		}
		if !headerSeen {
			continue
		}
		row, ok := parseShortRow(line)
		if !ok {
			continue
		}
		if prev, exists := best[row.Name]; exists {
			// prefer any 'active'
			if strings.EqualFold(row.Status, "active") && !strings.EqualFold(prev.Status, "active") {
				best[row.Name] = row.srcRow()
			}
			continue
		}
		best[row.Name] = row.srcRow()
	}
	return best, nil
}

// ----- lssrc -s <subsystem> (single) -----
func (c *aixSRCStatusCollector) runLssrcSingle(name string) (srcRow, error) {
	out, err := c.runCmd(c.lssrcPath, "-s", name)
	if err != nil {
		return srcRow{}, err
	}
	sc := newWideScanner(out)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		if lineNo == 2 { // data row
			if rec, ok := parseShortRow(strings.TrimSpace(sc.Text())); ok && rec.Name == name {
				return rec.srcRow(), nil
			}
			break
		}
	}
	return srcRow{Group: "unknown", PID: 0, Status: "absent"}, nil
}

// ---------- small exec utility ----------
func (c *aixSRCStatusCollector) runCmd(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// Force C locale for predictable columns.
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

// ---------- parsing ----------
type shortRow struct {
	Name   string
	Group  string
	PID    int
	Status string
}

var wsRE = regexp.MustCompile(`\s+`)

func parseShortRow(line string) (shortRow, bool) {
	// Expect: Subsystem Group PID Status
	// PID may be blank when inoperative.
	parts := wsRE.Split(strings.TrimSpace(line), -1)
	if len(parts) < 3 {
		return shortRow{}, false
	}
	// If exactly 3 fields, PID is blank => parts[0]=name, parts[1]=group, parts[2]=status
	if len(parts) == 3 {
		return shortRow{Name: parts[0], Group: parts[1], PID: 0, Status: strings.ToLower(parts[2])}, true
	}
	// >= 4 fields: take first 4
	pid := 0
	if p, err := parseInt(parts[2]); err == nil {
		pid = p
	}
	return shortRow{
		Name:   parts[0],
		Group:  parts[1],
		PID:    pid,
		Status: strings.ToLower(parts[3]),
	}, true
}

func (r shortRow) srcRow() srcRow { return srcRow{Group: r.Group, PID: r.PID, Status: r.Status} }

// ---------- small utils ----------
func newWideScanner(b []byte) *bufio.Scanner {
	sc := bufio.NewScanner(bytes.NewReader(b))
	buf := make([]byte, 0, 256*1024)
	sc.Buffer(buf, 1024*1024) // tolerate wide outputs
	return sc
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func parseInt(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}
