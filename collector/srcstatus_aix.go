// collector/srcstatus_aix.go
//go:build aix
// +build aix

package collector

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
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
	if c.mode == "bulk" || c.mode == "auto" {
		var err error
		bulk, err = c.runLssrcAll()
		if err != nil {
			if c.mode == "bulk" {
				// Emit nothing rather than reporting every monitored
				// subsystem as absent/down. `lssrc -a` failing or timing out
				// says nothing about the subsystems themselves, and a
				// fabricated 0 here pages the on-call for services that are
				// running fine. Surfaces instead as
				// node_scrape_collector_success{collector="aix_srcstatus"} == 0.
				return fmt.Errorf("lssrc -a: %w", err)
			}
			// auto: fall through to the per-subsystem path below.
			c.logger.Debug("lssrc -a failed, falling back to lssrc -s", "err", err)
		}
	}

	for _, name := range c.subsys {
		if row, ok := bulk[name]; ok {
			emitRow(ch, c.descUp, c.descPID, c.descInfo, name, row)
			continue
		}
		if c.mode == "single" || c.mode == "auto" {
			row, err := c.runLssrcSingle(name)
			if err != nil {
				// runLssrcSingle already resolved the one failure that means
				// "not defined" into a row, so anything left is SRC being
				// unable to answer. Emitting nothing beats reporting a running
				// subsystem as down.
				return fmt.Errorf("lssrc -s %s: %w", name, err)
			}
			emitRow(ch, c.descUp, c.descPID, c.descInfo, name, row)
			continue
		}
		// bulk succeeded but did not list this subsystem: it is not defined
		// in SRC on this host.
		emitRow(ch, c.descUp, c.descPID, c.descInfo, name, srcRow{Status: "absent"})
	}

	return nil
}

// ---------- Model & helpers ----------
type srcRow struct {
	Group  string
	PID    int
	Status string // "active" | "inoperative" | "starting" | "stopping" | "absent"
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
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	out, err := runCommand(ctx, c.lssrcPath, "-a")
	if err != nil {
		return nil, err
	}
	// Map name -> best row (prefer 'active' if duplicates exist).
	best := map[string]srcRow{}
	sc := newScanner(out)
	for sc.Scan() {
		row, ok := parseShortRow(sc.Text())
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

// srcNotOnFileRe matches SRC's "subsystem is not defined" diagnostic:
//
//	0513-085 The foo Subsystem is not on file.
//
// This is the one lssrc failure that genuinely means "down/absent". Every
// other non-zero exit — srcmstr unresponsive, a permission problem — says
// nothing about the subsystem and must not be reported as one.
var srcNotOnFileRe = regexp.MustCompile(`0513-085|[Nn]ot on file`)

// ----- lssrc -s <subsystem> (single) -----
func (c *aixSRCStatusCollector) runLssrcSingle(name string) (srcRow, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	// Tolerant: lssrc's exit status alone cannot distinguish "this subsystem
	// is not defined" from "SRC could not answer", so the output is needed
	// either way.
	out, err := runCommandTolerant(ctx, c.lssrcPath, "-s", name)
	if err != nil && isDeadline(err) {
		return srcRow{}, err
	}

	// Scan for the matching row rather than assuming it is line 2: a warning
	// or an SRC informational message ahead of the table would otherwise make
	// a running subsystem read as absent.
	sc := newScanner(out)
	for sc.Scan() {
		if rec, ok := parseShortRow(sc.Text()); ok && rec.Name == name {
			return rec.srcRow(), nil
		}
	}

	if srcNotOnFileRe.Match(out) {
		return srcRow{Status: "absent"}, nil
	}
	if err != nil {
		// An unexplained failure. Propagate it so Update declines to emit,
		// rather than reporting a running subsystem as absent.
		return srcRow{}, err
	}
	// lssrc succeeded and printed a table that did not mention this
	// subsystem: it is genuinely not defined here.
	return srcRow{Status: "absent"}, nil
}

// ---------- parsing ----------
type shortRow struct {
	Name   string
	Group  string
	PID    int
	Status string
}

func (r shortRow) srcRow() srcRow { return srcRow{Group: r.Group, PID: r.PID, Status: r.Status} }

// srcStates is the set of values SRC reports in the Status column. Requiring
// the last field to be one of these is what separates a data row from the
// table header, from blank lines, and from SRC's numbered diagnostics
// ("0513-004 The Subsystem or Group is currently inoperative."), which
// otherwise parse into phantom subsystems.
var srcStates = map[string]bool{
	"active":      true,
	"inoperative": true,
	"starting":    true,
	"stopping":    true,
}

// parseShortRow parses one row of `lssrc -a` / `lssrc -s` output.
//
// The columns are Subsystem, Group, PID, Status — but Group and PID are BOTH
// optional and are blank-padded rather than filled, so a row carries anywhere
// from two to four whitespace-separated fields:
//
//	syslogd          ras              10223970     active   -> group + pid
//	qdaemon          spooler                       inoperative -> group only
//	aso                               9306448      active   -> pid only
//	cdromd                                         inoperative -> neither
//
// Splitting on whitespace and reading fixed offsets (the previous approach)
// therefore mis-parses the last two shapes. A groupless-but-running subsystem
// like aso, gc-agent, ds_agent or node_exporter_aix_go had its PID read as the
// Group, reporting node_src_subsystem_pid=0 and putting the PID into the
// group label — where it changed on every restart, so each restart silently
// started a new time series. A groupless, stopped subsystem produced only two
// fields, was rejected outright, and got reported as "absent" instead of
// "inoperative".
//
// Anchoring on the ends instead is unambiguous: the name is always first and
// the status always last. What sits between them is a PID if it is numeric,
// and a group otherwise.
func parseShortRow(line string) (shortRow, bool) {
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return shortRow{}, false
	}

	status := strings.ToLower(parts[len(parts)-1])
	if !srcStates[status] {
		return shortRow{}, false
	}

	row := shortRow{Name: parts[0], Status: status}

	middle := parts[1 : len(parts)-1]
	if n := len(middle); n > 0 {
		if pid, err := strconv.Atoi(middle[n-1]); err == nil {
			row.PID = pid
			middle = middle[:n-1]
		}
	}
	if len(middle) > 0 {
		row.Group = middle[0]
	}

	return row, true
}

// ---------- small utils ----------
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
