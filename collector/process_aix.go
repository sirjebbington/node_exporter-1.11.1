// collector/process_aix.go
//go:build aix
// +build aix

package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/client_golang/prometheus"
)

// ---------- Flags ----------
var (
	processTimeout = kingpin.Flag(
		"collector.aix_process.timeout",
		"Timeout for ps invocation. Also capped by whatever is left of the collector's scrape budget.").
		Default("8s").Duration()

	processPsPath = kingpin.Flag(
		"collector.aix_process.ps-path",
		"Path to ps (leave empty to resolve via PATH).").
		Default("ps").String()

	processTargets = kingpin.Flag(
		"collector.aix_process.processes",
		"Comma-separated list of full executable paths to monitor (e.g. /usr/lib/errdemon,/usr/sbin/cron).").
		Default("/usr/lib/errdemon,/usr/sbin/cron").String()
)

// psFormatArgs asks ps for only the two columns the collector needs. The
// trailing '=' suppresses the headers, so every line is "PID COMMAND ARGS..."
// and the executable is always the second token: no column drift, and much
// less output to read than ps -ef on a host running thousands of processes.
var psFormatArgs = []string{"-eo", "pid=,args="}

// ---------- Registration ----------
func init() {
	registerCollector("aix_process", defaultDisabled, NewAIXProcessCollector)
}

// ---------- Collector ----------
type aixProcessCollector struct {
	logger  *slog.Logger
	timeout time.Duration
	psPath  string
	targets []string

	descUp  *prometheus.Desc
	descPID *prometheus.Desc
}

func NewAIXProcessCollector(logger *slog.Logger) (Collector, error) {
	if logger == nil {
		logger = slog.Default()
	}

	targets := splitCSV(*processTargets)

	ns := namespace
	return &aixProcessCollector{
		logger:  logger,
		timeout: *processTimeout,
		psPath:  *processPsPath,
		targets: targets,

		descUp: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "process", "up"),
			"Whether the process is running (1=running, 0=not found). Not published for a scrape in which ps did not answer.",
			[]string{"process"}, nil,
		),
		descPID: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "process", "pid"),
			"PID of the process when running, else 0.",
			[]string{"process"}, nil,
		),
	}, nil
}

// ---------- Update ----------

// Update publishes one up/pid pair per monitored process, but only for a
// scrape in which ps actually produced a process table.
//
// If ps times out, is killed with the scrape budget, or returns something
// unreadable, no process metrics are published at all. Emitting 0 there would
// report every monitored process as dead because the host was too busy to run
// ps — the one moment those alerts must not fire spuriously. Zero is reserved
// for its true meaning: ps listed the running processes and this one was not
// among them. The failure stays visible as node_scrape_collector_timeout and
// node_scrape_collector_success.
func (c *aixProcessCollector) Update(ch chan<- prometheus.Metric) error {
	if len(c.targets) == 0 {
		return nil
	}

	guard := NewTimeoutGuard("aix_process", c.logger, 0)
	ctx, cancel := guard.Context()
	defer cancel()

	found, err := c.collectProcesses(ctx, guard)
	guard.EmitIfTimedOut(ch)
	if err != nil {
		c.logger.Debug("aix_process: ps did not answer, leaving process series absent this scrape", "err", err)
		return err
	}

	for _, t := range c.targets {
		pid, running := found[t]
		up := 0.0
		if running {
			up = 1.0
		}
		ch <- prometheus.MustNewConstMetric(c.descUp, prometheus.GaugeValue, up, t)
		ch <- prometheus.MustNewConstMetric(c.descPID, prometheus.GaugeValue, float64(pid), t)
	}
	return nil
}

// collectProcesses returns a map of executable path -> PID for every
// monitored target that is running. An error means the process table could
// not be read, which is never the same thing as an empty map.
func (c *aixProcessCollector) collectProcesses(ctx context.Context, guard *TimeoutGuard) (map[string]int, error) {
	out, err := c.runPS(ctx, guard, psFormatArgs...)
	if err == nil {
		found, perr := c.scanPS(out, parsePSFormatRow)
		if perr == nil {
			return found, nil
		}
		err = perr
	}
	if isDeadline(err) {
		// Out of budget: retrying with another form would only spend time
		// this scrape no longer has.
		return nil, err
	}

	// A ps too old for -o, or one whose output the format parser could not
	// make sense of, still answers -ef.
	c.logger.Debug("aix_process: ps -eo unusable, falling back to ps -ef", "err", err)
	out, fallbackErr := c.runPS(ctx, guard, "-ef")
	if fallbackErr != nil {
		return nil, errors.Join(err, fallbackErr)
	}
	found, fallbackErr := c.scanPS(out, parsePSEFRow)
	if fallbackErr != nil {
		return nil, errors.Join(err, fallbackErr)
	}
	return found, nil
}

// scanPS walks the process table with the given row parser and picks out the
// monitored targets.
func (c *aixProcessCollector) scanPS(out []byte, parse func(string) (int, string, bool)) (map[string]int, error) {
	targetSet := make(map[string]bool, len(c.targets))
	for _, t := range c.targets {
		targetSet[t] = true
	}

	found := make(map[string]int, len(c.targets))
	rows := 0

	sc := newWideScanner(out)
	for sc.Scan() {
		pid, cmd, ok := parse(sc.Text())
		if !ok {
			continue
		}
		rows++
		if !targetSet[cmd] {
			continue
		}
		// Keep the lowest PID if multiple instances exist.
		if existing, seen := found[cmd]; !seen || pid < existing {
			found[cmd] = pid
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading ps output: %w", err)
	}
	if rows == 0 {
		// ps exited 0 but listed no process this parser could read. A running
		// AIX system always has processes, so this is an unreadable answer,
		// not proof that every target is gone.
		return nil, ErrNoData
	}
	return found, nil
}

// ---------- parsing ----------

// parsePSFormatRow reads a line of `ps -eo pid=,args=`:
//
//	123456 /usr/sbin/cron -s
//	     1 /etc/init
//
// The executable is the second token; everything after it is arguments.
func parsePSFormatRow(line string) (int, string, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, "", false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid < 0 {
		return 0, "", false
	}
	return pid, fields[1], true
}

// parsePSEFRow reads a line of `ps -ef`:
//
//	 UID    PID   PPID   C    STIME    TTY  TIME CMD
//	root 123456      1   0 09:12:33      -  0:00 /usr/sbin/cron -s
//	root      1      0   0   Sep 01      -  0:12 /etc/init
//
// STIME is one token (a clock time) for a process started today and two
// ("Sep 01") for an older one, which shifts CMD from field 7 to field 8. Only
// the first token of CMD is the executable: scanning the arguments too would
// report /usr/sbin/cron as running because something else ran
// `sh -c /usr/sbin/cron`, and would report that wrapper's PID.
func parsePSEFRow(line string) (int, string, bool) {
	fields := strings.Fields(line)
	if len(fields) < 8 {
		return 0, "", false
	}
	pid, err := strconv.Atoi(fields[1])
	if err != nil || pid < 0 {
		return 0, "", false // the header row, and anything malformed
	}
	cmdIdx := 7
	if isMonthAbbrev(fields[4]) {
		cmdIdx = 8
	}
	if cmdIdx >= len(fields) {
		return 0, "", false
	}
	return pid, fields[cmdIdx], true
}

// isMonthAbbrev reports whether s is a C-locale month abbreviation, which is
// how ps renders the STIME of a process that did not start today.
func isMonthAbbrev(s string) bool {
	switch s {
	case "Jan", "Feb", "Mar", "Apr", "May", "Jun",
		"Jul", "Aug", "Sep", "Oct", "Nov", "Dec":
		return true
	}
	return false
}

// ---------- exec ----------

// runPS bounds one ps call by the smaller of the per-invocation timeout and
// what is left of the collector's scrape budget.
func (c *aixProcessCollector) runPS(ctx context.Context, guard *TimeoutGuard, args ...string) ([]byte, error) {
	cctx, cancel := budgetedCommandContext(ctx, guard, c.timeout)
	defer cancel()

	out, err := runCommand(cctx, c.psPath, args...)
	if err != nil {
		if isDeadline(err) {
			guard.FlagTimeout("ps_timeout")
		}
		return nil, err
	}
	return out, nil
}
