// collector/srcstatus_aix.go
//go:build aix
// +build aix

package collector

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
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
		"Timeout per lssrc invocation. Also capped by whatever is left of the collector's scrape budget.").
		Default("8s").Duration()

	srcstatusPath = kingpin.Flag(
		"collector.aix_srcstatus.lssrc-path",
		"Path to lssrc (leave empty to resolve via PATH).").
		Default("lssrc").String()
)

const (
	srcModeBulk   = "bulk"
	srcModeSingle = "single"
	srcModeAuto   = "auto"
)

// Statuses SRC prints in the Status column, plus the one this collector adds
// for a subsystem SRC does not know about at all.
const (
	srcStatusActive = "active"
	srcStatusAbsent = "absent"
)

// unknownGroup labels a subsystem whose SRC group has never been observed.
const unknownGroup = "unknown"

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

	// groupMu guards groups, the last SRC group seen per subsystem. lssrc
	// leaves the Group column blank for some subsystems and stops listing one
	// entirely once it is removed, and the group is a metric label: without
	// this the series would change identity at exactly the moment an operator
	// is looking at it. Update may run concurrently with itself across
	// scrapes, so the map needs a lock.
	groupMu sync.Mutex
	groups  map[string]string

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
		groups:    make(map[string]string, len(targets)),
		descUp: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "src", "subsystem_up"),
			"SRC subsystem up (1=active, 0=not active/inoperative/absent). Not published for a scrape in which lssrc did not answer.",
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

// Update publishes one set of metrics per subsystem lssrc actually reported
// on this scrape.
//
// A subsystem whose state could not be established — lssrc timed out, was
// killed with the scrape budget, or failed for any reason other than telling
// us the subsystem does not exist — is left out entirely rather than
// published as 0. Zero means "SRC answered and the subsystem is not running";
// absence means "nobody knows", which is what a timeout actually tells us.
// The failure itself is still visible, as node_scrape_collector_timeout and
// node_scrape_collector_success.
func (c *aixSRCStatusCollector) Update(ch chan<- prometheus.Metric) error {
	if len(c.subsys) == 0 {
		return nil
	}

	guard := NewTimeoutGuard("aix_srcstatus", c.logger, 0)
	ctx, cancel := guard.Context()
	defer cancel()

	rows, errs := c.observe(ctx, guard)
	guard.EmitIfTimedOut(ch)

	if len(rows) == 0 {
		// Nothing was observed, so there is nothing truthful to publish.
		if len(errs) == 0 {
			return ErrNoData
		}
		return errors.Join(errs...)
	}

	for _, name := range c.subsys {
		row, ok := rows[name]
		if !ok {
			continue
		}
		c.emit(ch, name, row)
	}

	// A partial reading still publishes what it learned: the subsystems that
	// did answer keep their series, and only the unresolved ones go missing.
	if len(errs) > 0 {
		c.logger.Debug("aix_srcstatus: some subsystems were not resolved this scrape",
			"observed", len(rows), "targets", len(c.subsys), "err", errors.Join(errs...))
	}
	return nil
}

// observe resolves as many target subsystems as it can within the budget. A
// name missing from the returned map was not observed and must not be
// published.
func (c *aixSRCStatusCollector) observe(ctx context.Context, guard *TimeoutGuard) (map[string]srcRow, []error) {
	rows := make(map[string]srcRow, len(c.subsys))
	var errs []error

	if c.mode == srcModeBulk || c.mode == srcModeAuto {
		bulk, err := c.runLssrcAll(ctx, guard)
		switch {
		case err != nil:
			errs = append(errs, err)
		case len(bulk) == 0:
			// lssrc exited 0 but printed no parseable row. SRC always lists
			// its own subsystems on a healthy host, so an empty table is an
			// unreadable answer, not proof that every subsystem is gone.
			errs = append(errs, errors.New("lssrc -a returned no subsystem rows"))
		default:
			for _, name := range c.subsys {
				if row, ok := bulk[name]; ok {
					rows[name] = row
					continue
				}
				if c.mode == srcModeBulk {
					// The listing succeeded and does not mention it: SRC has
					// no such subsystem. That is a real observation, so it is
					// published as down rather than dropped.
					rows[name] = srcRow{Status: srcStatusAbsent}
				}
			}
		}
	}

	if c.mode == srcModeBulk {
		return rows, errs
	}

	for _, name := range c.subsys {
		if _, done := rows[name]; done {
			continue
		}
		row, err := c.runLssrcSingle(ctx, guard, name)
		if err != nil {
			// Unresolved: leave the series absent for this scrape.
			errs = append(errs, err)
			continue
		}
		rows[name] = row
	}
	return rows, errs
}

// ---------- Model & helpers ----------
type srcRow struct {
	Group  string
	PID    int
	Status string // "active" | "inoperative" | "absent" | whatever SRC printed
}

func (r srcRow) up() float64 {
	if strings.EqualFold(r.Status, srcStatusActive) {
		return 1
	}
	return 0
}

func (c *aixSRCStatusCollector) emit(ch chan<- prometheus.Metric, name string, row srcRow) {
	group := c.groupFor(name, row.Group)
	ch <- prometheus.MustNewConstMetric(c.descUp, prometheus.GaugeValue, row.up(), name, group)
	ch <- prometheus.MustNewConstMetric(c.descPID, prometheus.GaugeValue, float64(row.PID), name, group)
	ch <- prometheus.MustNewConstMetric(c.descInfo, prometheus.GaugeValue, 1, name, group, row.Status)
}

// groupFor keeps the group label stable across the transition that matters:
// an active subsystem carries its group, and when it goes inoperative or
// disappears from lssrc entirely the metric keeps the group it was last seen
// in instead of jumping to a second series labelled "unknown".
func (c *aixSRCStatusCollector) groupFor(name, observed string) string {
	c.groupMu.Lock()
	defer c.groupMu.Unlock()
	if observed != "" {
		c.groups[name] = observed
		return observed
	}
	if remembered, ok := c.groups[name]; ok {
		return remembered
	}
	return unknownGroup
}

// ----- lssrc -a (bulk) -----
func (c *aixSRCStatusCollector) runLssrcAll(ctx context.Context, guard *TimeoutGuard) (map[string]srcRow, error) {
	out, err := c.runRaw(ctx, guard, "lssrc_a_timeout", "-a")
	if err != nil {
		return nil, err
	}

	best := make(map[string]srcRow, 64)
	sc := newWideScanner(out)
	for sc.Scan() {
		name, row, ok := parseSRCRow(sc.Text())
		if !ok {
			continue
		}
		// A subsystem can be listed more than once (one row per instance);
		// prefer an active row so a single running instance counts as up.
		if prev, dup := best[name]; dup && (prev.up() == 1 || row.up() == 0) {
			continue
		}
		best[name] = row
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading lssrc -a output: %w", err)
	}
	return best, nil
}

// ----- lssrc -s <subsystem> (single) -----
func (c *aixSRCStatusCollector) runLssrcSingle(ctx context.Context, guard *TimeoutGuard, name string) (srcRow, error) {
	out, err := c.runRaw(ctx, guard, "lssrc_s_timeout", "-s", name)
	if err != nil {
		if isDeadline(err) {
			return srcRow{}, err
		}
		// A non-zero exit is only an answer when SRC said what was wrong: the
		// subsystem is not defined. Anything else (lssrc missing, SRC daemon
		// not reachable, permission denied) leaves the state unknown.
		if isSubsystemUndefined(out) {
			return srcRow{Status: srcStatusAbsent}, nil
		}
		return srcRow{}, err
	}

	sc := newWideScanner(out)
	for sc.Scan() {
		got, row, ok := parseSRCRow(sc.Text())
		if ok && got == name {
			return row, nil
		}
	}
	if isSubsystemUndefined(out) {
		return srcRow{Status: srcStatusAbsent}, nil
	}
	return srcRow{}, fmt.Errorf("lssrc -s %s returned no row for it: %s", name, firstLine(out))
}

// isSubsystemUndefined reports whether lssrc said the subsystem does not
// exist ("0513-085 The foo Subsystem is not on file."). That is a definitive
// answer — an undefined subsystem cannot be running — unlike a timeout, which
// says nothing at all.
func isSubsystemUndefined(out []byte) bool {
	s := strings.ToLower(string(out))
	return strings.Contains(s, "0513-085") || strings.Contains(s, "not on file")
}

// ---------- exec ----------

// runRaw bounds one lssrc call by the smaller of the per-invocation timeout
// and what is left of the collector's scrape budget, so a mode that shells out
// once per subsystem cannot walk past the budget one timeout at a time.
//
// It hands back whatever lssrc printed alongside the error, so the caller can
// tell "no such subsystem" apart from "lssrc did not answer".
func (c *aixSRCStatusCollector) runRaw(ctx context.Context, guard *TimeoutGuard, reason string, args ...string) ([]byte, error) {
	cctx, cancel := budgetedCommandContext(ctx, guard, c.timeout)
	defer cancel()

	out, err := runCommandRaw(cctx, c.lssrcPath, args...)
	if err != nil {
		if isDeadline(err) {
			guard.FlagTimeout(reason)
		}
		c.logger.Debug("aix_srcstatus: lssrc failed", "args", args, "err", err)
		return out, err
	}
	return out, nil
}

// ---------- parsing ----------

// parseSRCRow reads one row of lssrc short output:
//
//	Subsystem         Group            PID          Status
//	 syslogd          ras              123456       active
//	 xntpd            tcpip                         inoperative
//	 ctrmc                             234567       active
//
// Only Subsystem and Status are always filled in: an inoperative subsystem
// has no PID, and some subsystems belong to no group. Splitting on whitespace
// and reading columns left to right therefore mistakes a PID for a group on
// those rows, so the fields are read from the outside in — name first, status
// last, then the PID if what remains ends in digits.
func parseSRCRow(line string) (name string, row srcRow, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] == "Subsystem" {
		return "", srcRow{}, false
	}

	name = fields[0]
	status := strings.ToLower(fields[len(fields)-1])
	middle := fields[1 : len(fields)-1]

	pid := 0
	if n := len(middle); n > 0 {
		// Group names are never all digits, so a numeric last column is the
		// PID and anything else means the PID column was blank.
		if v, err := strconv.Atoi(middle[n-1]); err == nil {
			pid = v
			middle = middle[:n-1]
		}
	}

	group := ""
	if len(middle) > 0 {
		group = middle[0]
	}
	return name, srcRow{Group: group, PID: pid, Status: status}, true
}

// ---------- small utils ----------

// budgetedCommandContext bounds one command by the smaller of want and the
// time the collector has left, reserving cmdWaitDelay on top of it.
//
// A command that has to be killed is not finished when the kill lands:
// Cmd.Wait still drains its pipes for up to cmdWaitDelay afterwards, because
// an orphaned child can hold the write end open (see cmdWaitDelay in
// fs_nfs_aix.go). Without the reservation an 8s command timeout inside a 9s
// budget returns at ~10s, past the scrape deadline — and a scrape that times
// out loses every collector's metrics, not just this one's.
func budgetedCommandContext(ctx context.Context, guard *TimeoutGuard, want time.Duration) (context.Context, context.CancelFunc) {
	if room := guard.TimeRemaining() - cmdWaitDelay; room > 0 && room < want {
		want = room
	}
	return context.WithTimeout(ctx, want)
}

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
