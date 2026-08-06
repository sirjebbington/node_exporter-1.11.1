// collector/process_aix.go
//go:build aix
// +build aix

package collector

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
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
		"Comma-separated list of full executable paths to monitor (e.g. /usr/lib/errdemon,/usr/sbin/cron).").
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
			"Whether the process is running (1=running, 0=not found).",
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

func (c *aixProcessCollector) Update(ch chan<- prometheus.Metric) error {
	if len(c.targets) == 0 {
		return nil
	}

	found, err := c.collectProcesses()
	if err != nil {
		c.logger.Debug("ps collection failed", "err", err)
		// Emit 0 for all targets so dashboards show down rather than missing.
		for _, t := range c.targets {
			ch <- prometheus.MustNewConstMetric(c.descUp, prometheus.GaugeValue, 0, t)
			ch <- prometheus.MustNewConstMetric(c.descPID, prometheus.GaugeValue, 0, t)
		}
		return nil
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

// collectProcesses runs ps -ef and returns a map of executable path -> PID
// for any process whose CMD field exactly matches a monitored target.
func (c *aixProcessCollector) collectProcesses() (map[string]int, error) {
	out, err := c.runCmd(c.psPath, "-ef")
	if err != nil {
		return nil, err
	}

	// Build a set for O(1) lookups.
	targetSet := make(map[string]bool, len(c.targets))
	for _, t := range c.targets {
		targetSet[t] = true
	}

	result := make(map[string]int)

	for _, rawLine := range strings.Split(string(out), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}

		// AIX ps -ef columns (whitespace-separated):
		//   [0]=UID [1]=PID [2]=PPID [3]=C [4]=STIME [5]=TTY [6]=TIME [7]=CMD [8+]=args
		// STIME is "HH:MM" (1 token) for today's processes or "Mon DD" (2 tokens) for
		// older ones, shifting CMD from index 7 to index 8. Search from index 7 onward.
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}

		cmd := ""
		for _, f := range fields[7:] {
			if targetSet[f] {
				cmd = f
				break
			}
		}
		if cmd == "" {
			continue
		}

		pid := 0
		fmt.Sscanf(fields[1], "%d", &pid)

		// Keep lowest PID if multiple instances exist.
		if existing, seen := result[cmd]; !seen || pid < existing {
			result[cmd] = pid
		}
	}

	return result, nil
}

// ---------- exec helper ----------

func (c *aixProcessCollector) runCmd(name string, args ...string) ([]byte, error) {
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
