// collector/process_aix.go
//go:build aix
// +build aix

package collector

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"regexp"
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
		"Timeout for ps invocation.").
		Default("3s").Duration()

	processPsPath = kingpin.Flag(
		"collector.aix_process.ps-path",
		"Path to ps (leave empty to resolve via PATH).").
		Default("ps").String()

	processTargets = kingpin.Flag(
		"collector.aix_process.processes",
		"Comma-separated list of processes to monitor. An entry containing a '/' is matched against the process's full executable path; an entry without one is matched against the executable's base name (e.g. /usr/sbin/cron, or just cron).").
		Default("/usr/lib/errdemon,/usr/sbin/cron").String()
)

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

	descUp        *prometheus.Desc
	descPID       *prometheus.Desc
	descInstances *prometheus.Desc
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
			"Whether the process is running (1=running, 0=not found).",
			[]string{"process"}, nil,
		),
		descPID: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "process", "pid"),
			"PID of the process when running (lowest PID if several match), else 0.",
			[]string{"process"}, nil,
		),
		descInstances: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "process", "instances"),
			"Number of running processes matching this entry.",
			[]string{"process"}, nil,
		),
	}, nil
}

// ---------- Update ----------

func (c *aixProcessCollector) Update(ch chan<- prometheus.Metric) error {
	if len(c.targets) == 0 {
		return nil
	}

	found, err := c.collectProcesses()
	if err != nil {
		// Deliberately emit nothing rather than a fabricated 0 for every
		// target. A failed or timed-out `ps` says nothing about whether the
		// daemons are running, and reporting them all as down turns an
		// exporter-side problem into a fleet-wide false "service down" page.
		// The failure is still visible, as
		// node_scrape_collector_success{collector="aix_process"} == 0.
		return fmt.Errorf("ps: %w", err)
	}

	for _, t := range c.targets {
		m := found[t]
		up := 0.0
		if m.count > 0 {
			up = 1.0
		}
		ch <- prometheus.MustNewConstMetric(c.descUp, prometheus.GaugeValue, up, t)
		ch <- prometheus.MustNewConstMetric(c.descPID, prometheus.GaugeValue, float64(m.pid), t)
		ch <- prometheus.MustNewConstMetric(c.descInstances, prometheus.GaugeValue, float64(m.count), t)
	}
	return nil
}

// procMatch accumulates the running processes matching one configured target.
type procMatch struct {
	pid   int
	count int
}

// collectProcesses returns, per configured target, the lowest matching PID and
// how many processes matched.
//
// Matching considers ONLY argv[0] — the executable the process was started as.
// The previous implementation scanned every whitespace-separated field from
// index 7 to end of line, which is the command PLUS all of its arguments, so
// any unrelated process that merely mentioned a monitored path in its command
// line marked that target as up. `grep /usr/sbin/cron ...`, a backup agent
// invoked as `wrapper -c /usr/lib/errdemon`, or an admin's editor session were
// all enough to report a stopped daemon as running, with the impostor's PID.
func (c *aixProcessCollector) collectProcesses() (map[string]procMatch, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	// `ps -eo pid=,args=` yields exactly two columns — PID and the full command
	// line — which removes the column-counting guesswork below entirely. The
	// trailing '=' suppresses the header.
	var rows []psRow
	out, err := runCommand(ctx, c.psPath, "-eo", "pid=,args=")
	if err == nil {
		rows = parsePSTwoColumn(out)
	} else if isDeadline(err) {
		return nil, err
	}

	// Fall back to plain `ps -ef` if this AIX level's ps rejects -o, or
	// accepted it but printed something this parser could not read. No host
	// runs zero processes, so an empty result means the output was not what
	// was expected — falling back beats reporting every daemon down.
	if len(rows) == 0 {
		c.logger.Debug("ps -eo unusable, falling back to ps -ef", "err", err)
		out, err = runCommand(ctx, c.psPath, "-ef")
		if err != nil {
			return nil, err
		}
		rows = parsePSDashEF(out)
	}

	// Index the targets so each line is matched in constant time. Entries
	// containing a separator match the full path; bare names match the base
	// name, which is what lets a daemon started via a relative path or a
	// symlink still be found.
	byPath := make(map[string]string, len(c.targets))
	byBase := make(map[string]string, len(c.targets))
	for _, t := range c.targets {
		if strings.Contains(t, "/") {
			byPath[t] = t
		} else {
			byBase[t] = t
		}
	}

	result := make(map[string]procMatch, len(c.targets))
	for _, p := range rows {
		target, ok := byPath[p.argv0]
		if !ok {
			target, ok = byBase[path.Base(p.argv0)]
		}
		if !ok {
			continue
		}
		m := result[target]
		m.count++
		if m.pid == 0 || p.pid < m.pid {
			m.pid = p.pid
		}
		result[target] = m
	}

	return result, nil
}

// psRow is one process: its PID and the executable it was started as.
type psRow struct {
	pid   int
	argv0 string
}

// parsePSTwoColumn parses `ps -eo pid=,args=`:
//
//	12517786 /usr/sbin/cron
//	14614952 /perfdata/node_exporter/node_exporter_aix_ppc64 --web.listen-address=:9681
//
// Anything whose first field is not numeric is skipped, which also discards a
// header row on any AIX level that ignores the '=' suffix.
func parsePSTwoColumn(out []byte) []psRow {
	var rows []psRow
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		rows = append(rows, psRow{pid: pid, argv0: fields[1]})
	}
	return rows
}

// psTimeRe matches the TIME column of `ps -ef` output — cumulative CPU time,
// always "M:SS" or "MMM:SS". It deliberately does not match the "HH:MM:SS"
// form the STIME column takes for a process started today, which is what makes
// it usable as a landmark.
var psTimeRe = regexp.MustCompile(`^[0-9]+:[0-9]{2}$`)

// parsePSDashEF parses `ps -ef`, whose columns are:
//
//	UID PID PPID C STIME TTY TIME CMD [args...]
//
// STIME is one token for a process started today ("18:25:36") but two for an
// older one ("Jul 29"), so CMD sits at index 7 or 8 depending on the process's
// age:
//
//	root 14614952 25559546 0 18:25:36 pts/1 0:00 ./node_exporter --web...
//	root 12714390  8454550 0   Jul 29     - 0:15 /usr/local/bin/node_exporter_aix -cmda
//
// Rather than guess, locate the TIME column — the first token at index 4 or
// beyond matching psTimeRe — and take CMD as the token right after it.
func parsePSDashEF(out []byte) []psRow {
	var rows []psRow
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		pid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue // header row, or a wrapped line
		}
		cmd := -1
		for i := 4; i < len(fields)-1; i++ {
			if psTimeRe.MatchString(fields[i]) {
				cmd = i + 1
				break
			}
		}
		if cmd < 0 {
			continue
		}
		rows = append(rows, psRow{pid: pid, argv0: fields[cmd]})
	}
	return rows
}
