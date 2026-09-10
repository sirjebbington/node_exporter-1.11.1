// collector/timex_aix.go
//go:build aix
// +build aix

// AIX counterpart of the Linux timex collector (timex.go).
//
// timex.go reads the kernel clock discipline directly with adjtimex(2). That
// call is a Linux system call: AIX has no adjtimex, golang.org/x/sys/unix
// declares Timex as an empty struct on aix/ppc64 and exposes no Adjtimex, and
// libperfstat covers CPU, memory, disk, network, LVM and LPAR data but nothing
// about the clock. (AIX does ship a "timex" command, but it is the SVR4 one
// that times a command's execution — unrelated to this collector.)
//
// What AIX does have is the NTP daemon's own view of the discipline, which is
// where the kernel numbers come from in the first place. Two ways to ask it:
//
//   - ntpq -c "rv 0": the mode 6 system variables. Rich — offset, frequency,
//     jitter, wander, root delay and dispersion, stratum, leap. This is the
//     primary source. On AIX 7.3 NTPv3 is gone and /usr/sbin/ntpq is a symlink
//     to /usr/sbin/ntp4/ntpq4, so the output is the NTPv4 format; the older
//     v3 variable names are still accepted here for 7.1/7.2 hosts.
//   - lssrc -ls xntpd: SRC asks the daemon over its own channel, so it still
//     answers on hosts whose ntp.conf carries "restrict default noquery" and
//     refuses mode 6 queries. Coarser — no offset — but it also reports when
//     the subsystem is not running at all, which is itself a definitive
//     "not synchronised".
//
// Metric names match timex.go wherever AIX can supply the same quantity, so
// existing dashboards and alerts (node_timex_sync_status,
// node_timex_offset_seconds, ...) work unchanged across a mixed fleet. Metrics
// that only exist behind the kernel API are simply absent rather than faked:
// tick_seconds, the pps_* family, and node_timex_status, whose bits are the
// Linux STA_* flags and mean something entirely different in NTP's status
// word. The AIX-only additions (stratum, leap, root delay/dispersion, jitter,
// wander, precision) are named so they cannot collide with the cross-platform
// ntp collector's node_ntp_* series if both are enabled.

package collector

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/client_golang/prometheus"
)

// ----------------------------------------------------------------------------
// Flags
// ----------------------------------------------------------------------------

const (
	timexSourceAuto  = "auto"
	timexSourceNTPQ  = "ntpq"
	timexSourceLssrc = "lssrc"
)

var (
	aixTimexSource = kingpin.Flag(
		"collector.aix_timex.source",
		"Where to read clock discipline state: ntpq (mode 6 query), lssrc (SRC status of xntpd), or auto (ntpq, falling back to lssrc).",
	).Default(timexSourceAuto).Enum(timexSourceAuto, timexSourceNTPQ, timexSourceLssrc)

	aixTimexNTPQPath = kingpin.Flag(
		"collector.aix_timex.ntpq-path",
		"Path to ntpq (leave as the bare name to resolve via PATH; /usr/sbin/ntpq is the NTPv4 symlink on AIX 7.3).",
	).Default("ntpq").String()

	aixTimexNTPQHost = kingpin.Flag(
		"collector.aix_timex.ntpq-host",
		"Host ntpq queries. Only the local daemon's discipline of this machine's clock is meaningful here.",
	).Default("127.0.0.1").String()

	aixTimexLssrcPath = kingpin.Flag(
		"collector.aix_timex.lssrc-path",
		"Path to lssrc (leave as the bare name to resolve via PATH).",
	).Default("lssrc").String()

	aixTimexSubsystem = kingpin.Flag(
		"collector.aix_timex.subsystem",
		"SRC subsystem name of the NTP daemon, queried in lssrc mode. Still xntpd on AIX 7.3, where it runs the NTPv4 binary.",
	).Default("xntpd").String()

	// ntpq's own retry loop is ~5s per try, so without a shorter bound of our
	// own a daemon that ignores mode 6 queries would eat most of the scrape
	// budget before the lssrc fallback ever runs.
	aixTimexTimeout = kingpin.Flag(
		"collector.aix_timex.timeout",
		"Timeout per ntpq/lssrc invocation.",
	).Default("3s").Duration()
)

// ----------------------------------------------------------------------------
// Registration
// ----------------------------------------------------------------------------

func init() {
	registerCollector("aix_timex", defaultDisabled, NewAIXTimexCollector)
}

type aixTimexCollector struct {
	// Same names and meanings as timex.go.
	syncStatus,
	offset,
	freq,
	maxerror,
	esterror,
	constant,
	tai,
	// AIX additions, from the daemon's system variables.
	stratum,
	leap,
	rootDelay,
	rootDispersion,
	sysJitter,
	clockWander,
	precision typedDesc

	sourceInfo *prometheus.Desc

	logger    *slog.Logger
	source    string
	ntpqPath  string
	ntpqHost  string
	lssrcPath string
	subsystem string
	timeout   time.Duration
}

// NewAIXTimexCollector returns a Collector exposing the local NTP daemon's
// clock discipline state.
func NewAIXTimexCollector(logger *slog.Logger) (Collector, error) {
	if logger == nil {
		logger = slog.Default()
	}
	const subsystem = "timex"

	gauge := func(name, help string) typedDesc {
		return typedDesc{prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, name),
			help, nil, nil,
		), prometheus.GaugeValue}
	}

	return &aixTimexCollector{
		syncStatus: gauge("sync_status",
			"Is clock synchronized to a reliable server (1 = yes, 0 = no)."),
		offset: gauge("offset_seconds",
			"Time offset in between local system and reference clock."),
		freq: gauge("frequency_adjustment_ratio",
			"Local clock frequency adjustment."),
		maxerror: gauge("maxerror_seconds",
			"Maximum error in seconds (NTP root distance: rootdelay/2 + rootdisp)."),
		esterror: gauge("estimated_error_seconds",
			"Estimated error in seconds (clock jitter as estimated by the NTP daemon)."),
		constant: gauge("loop_time_constant",
			"Phase-locked loop time constant."),
		tai: gauge("tai_offset_seconds",
			"International Atomic Time (TAI) offset."),
		stratum: gauge("stratum",
			"Stratum of the local clock (16 = unsynchronized)."),
		leap: gauge("leap",
			"NTP leap indicator (0 = none, 1 = add second, 2 = delete second, 3 = unsynchronized)."),
		rootDelay: gauge("root_delay_seconds",
			"Total round-trip delay to the primary reference clock."),
		rootDispersion: gauge("root_dispersion_seconds",
			"Total dispersion to the primary reference clock."),
		sysJitter: gauge("sys_jitter_seconds",
			"Combined system jitter, the offset spread across the selected peers."),
		clockWander: gauge("clock_wander_ppm",
			"Clock frequency wander, the stability of recent frequency changes."),
		precision: gauge("precision_seconds",
			"Precision of the local clock, the resolution of a single clock read."),
		sourceInfo: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "source_info"),
			"Which command supplied this scrape's values, and the NTP daemon version if it reported one.",
			[]string{"source", "version"}, nil,
		),

		logger:    logger,
		source:    *aixTimexSource,
		ntpqPath:  *aixTimexNTPQPath,
		ntpqHost:  *aixTimexNTPQHost,
		lssrcPath: *aixTimexLssrcPath,
		subsystem: *aixTimexSubsystem,
		timeout:   *aixTimexTimeout,
	}, nil
}

func (c *aixTimexCollector) Update(ch chan<- prometheus.Metric) error {
	guard := NewTimeoutGuard("aix_timex", c.logger, 0)
	ctx, cancel := guard.Context()
	defer cancel()

	v, err := c.read(ctx, guard)
	guard.EmitIfTimedOut(ch)
	if err != nil {
		return err
	}

	// A variable the daemon did not report is skipped rather than published
	// as 0: a missing offset must not read as a perfectly disciplined clock.
	if sync, ok := v.syncStatus(); ok {
		ch <- c.syncStatus.mustNewConstMetric(sync)
	}
	emit := func(d typedDesc, key string) {
		if f, ok := v.get(key); ok {
			ch <- d.mustNewConstMetric(f)
		}
	}
	emit(c.offset, ntpVarOffsetSeconds)
	emit(c.constant, ntpVarLoopTimeConstant)
	emit(c.tai, ntpVarTAISeconds)
	emit(c.stratum, ntpVarStratum)
	emit(c.leap, ntpVarLeap)
	emit(c.rootDelay, ntpVarRootDelaySeconds)
	emit(c.rootDispersion, ntpVarRootDispSeconds)
	emit(c.sysJitter, ntpVarSysJitterSeconds)
	emit(c.clockWander, ntpVarClockWanderPPM)
	emit(c.precision, ntpVarPrecisionSeconds)

	// timex.go reports the frequency adjustment as a ratio around 1, derived
	// from the kernel's ppm value; the daemon reports the same correction in
	// ppm directly.
	if ppm, ok := v.get(ntpVarFrequencyPPM); ok {
		ch <- c.freq.mustNewConstMetric(1 + ppm/1e6)
	}

	// The kernel's maxerror is seeded from, and grows with, the NTP root
	// distance, which is what the daemon reports as its two components.
	delay, haveDelay := v.get(ntpVarRootDelaySeconds)
	disp, haveDisp := v.get(ntpVarRootDispSeconds)
	if haveDelay && haveDisp {
		ch <- c.maxerror.mustNewConstMetric(delay/2 + disp)
	}

	// Kernel esterror tracks the daemon's dispersion estimate; clk_jitter is
	// that estimate, with sys_jitter as the fallback for a daemon too old to
	// report it separately.
	if est, ok := v.get(ntpVarClkJitterSeconds); ok {
		ch <- c.esterror.mustNewConstMetric(est)
	} else if est, ok := v.get(ntpVarSysJitterSeconds); ok {
		ch <- c.esterror.mustNewConstMetric(est)
	}

	ch <- prometheus.MustNewConstMetric(c.sourceInfo, prometheus.GaugeValue, 1, v.source, v.version)
	return nil
}

// read tries the configured sources in order and returns the first reading
// that carries anything.
func (c *aixTimexCollector) read(ctx context.Context, guard *TimeoutGuard) (ntpSysVars, error) {
	var errs []error

	if c.source == timexSourceAuto || c.source == timexSourceNTPQ {
		out, err := c.run(ctx, guard, "ntpq_timeout", c.ntpqPath, "-n", "-c", "rv 0", c.ntpqHost)
		switch {
		case err != nil:
			errs = append(errs, err)
		default:
			// ntpq exits 0 even when it prints "***Request timed out", so
			// the parse result, not the exit status, decides whether the
			// daemon actually answered.
			if v := parseNTPQSysVars(out); !v.empty() {
				return v, nil
			}
			errs = append(errs, fmt.Errorf("ntpq returned no system variables: %s", firstLine(out)))
		}
	}

	if c.source == timexSourceAuto || c.source == timexSourceLssrc {
		out, err := c.run(ctx, guard, "lssrc_timeout", c.lssrcPath, "-ls", c.subsystem)
		switch {
		case err != nil:
			errs = append(errs, err)
		default:
			if v := parseLssrcXNTPD(out, c.subsystem); !v.empty() {
				return v, nil
			}
			errs = append(errs, fmt.Errorf("lssrc -ls %s returned no clock state: %s", c.subsystem, firstLine(out)))
		}
	}

	if len(errs) == 0 {
		return ntpSysVars{}, ErrNoData
	}
	return ntpSysVars{}, errors.Join(errs...)
}

// run bounds one command by the smaller of the per-command timeout and what is
// left of the collector's scrape budget.
func (c *aixTimexCollector) run(ctx context.Context, guard *TimeoutGuard, reason, name string, args ...string) ([]byte, error) {
	cctx, cancel := budgetedCommandContext(ctx, guard, c.timeout)
	defer cancel()

	out, err := runCommand(cctx, name, args...)
	if err != nil {
		if isDeadline(err) {
			guard.FlagTimeout(reason)
		}
		c.logger.Debug("aix_timex source failed", "command", name, "err", err)
		return nil, err
	}
	return out, nil
}

func firstLine(out []byte) string {
	line, _, _ := bytes.Cut(bytes.TrimSpace(out), []byte("\n"))
	return string(line)
}

// ----------------------------------------------------------------------------
// NTP system variables
// ----------------------------------------------------------------------------

// Canonical names for the variables the collector understands. Both parsers
// normalise into these units — seconds and ppm — so the emit path does not
// care whether the numbers arrived from ntpq (milliseconds) or lssrc
// (seconds).
const (
	ntpVarOffsetSeconds    = "offset_seconds"
	ntpVarFrequencyPPM     = "frequency_ppm"
	ntpVarRootDelaySeconds = "root_delay_seconds"
	ntpVarRootDispSeconds  = "root_dispersion_seconds"
	ntpVarSysJitterSeconds = "sys_jitter_seconds"
	ntpVarClkJitterSeconds = "clk_jitter_seconds"
	ntpVarClockWanderPPM   = "clock_wander_ppm"
	ntpVarPrecisionSeconds = "precision_seconds"
	ntpVarStratum          = "stratum"
	ntpVarLeap             = "leap"
	ntpVarLoopTimeConstant = "loop_time_constant"
	ntpVarTAISeconds       = "tai_offset_seconds"
)

// leapAlarm is the leap indicator a daemon sets when it has never
// synchronised, or has lost synchronisation (RFC 5905 §7.3).
const leapAlarm = 3

// unsyncStratum is the stratum reported by an unsynchronised server.
const unsyncStratum = 16

// ntpSysVars is one reading of the local NTP daemon's system variables.
type ntpSysVars struct {
	// source names the command that produced the reading.
	source string
	// version is the daemon's version string, when the source reports one.
	version string
	// vals holds only the variables actually present in the output, keyed by
	// the ntpVar* constants.
	vals map[string]float64
	// srcStatus is the SRC subsystem state from lssrc ("active",
	// "inoperative"); empty when the reading came from ntpq.
	srcStatus string
}

func newNTPSysVars(source string) ntpSysVars {
	return ntpSysVars{source: source, vals: make(map[string]float64, 12)}
}

func (v ntpSysVars) get(key string) (float64, bool) {
	f, ok := v.vals[key]
	return f, ok
}

func (v ntpSysVars) set(key string, f float64) {
	if v.vals != nil {
		v.vals[key] = f
	}
}

// setIfAbsent keeps the first value seen for a key, which is how the older
// NTPv3 aliases (phase, freq, stability, ...) act as fallbacks for their v4
// equivalents instead of overwriting them.
func (v ntpSysVars) setIfAbsent(key string, f float64) {
	if _, ok := v.vals[key]; !ok {
		v.set(key, f)
	}
}

// setIfPopulated stores num unless it is exactly zero.
//
// AIX's SRC status handler does not fill in every field it prints. On a
// synchronised 7.x host the NTPv4 daemon reports real numbers to ntpq while
// lssrc, at the same moment, still prints
//
//	Root distance:   0.000000
//	Root dispersion: 0.000000
//	Clock stability: 0.000000 (sec)
//	Clock frequency:
//
// with the blank frequency line giving the game away: these are unfilled
// placeholders, not measurements. None of these quantities can be exactly
// zero on a clock genuinely disciplined to a remote server — root dispersion
// alone grows continuously between updates — so an exact zero is read as "not
// reported" and the metric is left absent.
//
// It matters because these feed node_timex_maxerror_seconds, and a maxerror
// of 0 is the most misleading number this collector could publish: it says
// the clock is perfect at exactly the moment nothing is actually known.
func (v ntpSysVars) setIfPopulated(key string, f float64) {
	if f == 0 {
		return
	}
	v.set(key, f)
}

// empty reports whether the reading carries nothing worth publishing.
func (v ntpSysVars) empty() bool {
	return len(v.vals) == 0 && v.srcStatus == ""
}

// syncStatus mirrors timex.go's node_timex_sync_status: 1 when the clock is
// disciplined to a reliable server, 0 when it is not.
//
// The kernel expresses that as the TIME_ERROR state; a daemon reports the same
// condition as leap=3 (alarm) or stratum 16, and SRC reports it as a subsystem
// that is not running at all. The second return value is false when the output
// said nothing either way, so an incomplete reading is not published as
// "unsynchronised".
func (v ntpSysVars) syncStatus() (float64, bool) {
	if v.srcStatus != "" && !strings.EqualFold(v.srcStatus, "active") {
		return 0, true
	}
	leap, haveLeap := v.get(ntpVarLeap)
	stratum, haveStratum := v.get(ntpVarStratum)
	if !haveLeap && !haveStratum {
		return 0, false
	}
	if haveLeap && leap == leapAlarm {
		return 0, true
	}
	if haveStratum && (stratum <= 0 || stratum >= unsyncStratum) {
		return 0, true
	}
	return 1, true
}

// parseNTPQSysVars parses the system variables from `ntpq -c "rv 0"`.
//
// The mode 6 readout is a comma-separated list of name=value pairs wrapped
// across lines, mixed with bare status words and with two values that contain
// spaces and commas of their own:
//
//	associd=0 status=0615 leap_none, sync_ntp, 1 event, clock_sync,
//	version="ntpd 4.2.8p18@1.4062-o Tue Feb  4 08:12:33 UTC 2025 (1)",
//	processor="powerpc", system="AIX/7.3", leap=00, stratum=3,
//	precision=-20, rootdelay=12.345, rootdisp=45.678, refid=10.70.20.1,
//	reftime=eb0a9b1c.5e5b4a2f  Mon, Sep  1 2026 10:00:00.071, tc=10,
//	offset=0.123456, frequency=-12.345, sys_jitter=0.234567,
//	clk_jitter=0.123, clk_wander=0.012
//
// so it is scanned as quote-aware tokens rather than split on commas: anything
// that is not a name=value token — the flag words, the weekday and date fields
// spilling out of reftime — is dropped instead of being misread as a value.
//
// ntpq reports every time value in milliseconds, every frequency value in ppm,
// and precision as a power of two seconds.
func parseNTPQSysVars(out []byte) ntpSysVars {
	v := newNTPSysVars(timexSourceNTPQ)

	for _, tok := range splitNTPQTokens(string(out)) {
		key, raw, ok := strings.Cut(tok, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}

		if key == "version" {
			if v.version == "" {
				v.version = raw
			}
			continue
		}

		// The status word is the fallback for the leap indicator: its top two
		// bits carry the same value as leap= (RFC 5905 §7.3), and it is
		// present even in the terse output of an old daemon.
		if key == "status" {
			if word, err := strconv.ParseUint(raw, 16, 16); err == nil {
				v.setIfAbsent(ntpVarLeap, float64((word>>14)&0x3))
			}
			continue
		}

		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			continue
		}

		switch key {
		case "offset": // v4, milliseconds
			v.set(ntpVarOffsetSeconds, f/1000)
		case "phase": // v3 name for the same quantity
			v.setIfAbsent(ntpVarOffsetSeconds, f/1000)
		case "frequency", "freq": // ppm
			v.setIfAbsent(ntpVarFrequencyPPM, f)
		case "rootdelay": // milliseconds
			v.set(ntpVarRootDelaySeconds, f/1000)
		case "rootdisp", "rootdispersion": // milliseconds
			v.setIfAbsent(ntpVarRootDispSeconds, f/1000)
		case "sys_jitter", "jitter": // milliseconds
			v.setIfAbsent(ntpVarSysJitterSeconds, f/1000)
		case "clk_jitter", "error": // milliseconds
			v.setIfAbsent(ntpVarClkJitterSeconds, f/1000)
		case "clk_wander", "stability": // ppm
			v.setIfAbsent(ntpVarClockWanderPPM, f)
		case "precision": // log2 seconds
			v.set(ntpVarPrecisionSeconds, math.Exp2(f))
		case "stratum":
			v.set(ntpVarStratum, f)
		case "leap":
			// Printed zero padded, "00".."03"; the wire format is decimal.
			v.set(ntpVarLeap, f)
		case "tc":
			v.set(ntpVarLoopTimeConstant, f)
		case "tai":
			// Only present once a leap second file is loaded.
			v.set(ntpVarTAISeconds, f)
		}
	}

	return v
}

// splitNTPQTokens splits mode 6 output on whitespace and commas, treating a
// double-quoted run as a single token so that version="ntpd 4.2.8p18 ..."
// survives intact.
func splitNTPQTokens(s string) []string {
	var (
		toks  []string
		b     strings.Builder
		quote bool
	)
	flush := func() {
		if b.Len() > 0 {
			toks = append(toks, b.String())
			b.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '"':
			quote = !quote
		case !quote && (r == ',' || unicode.IsSpace(r)):
			flush()
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return toks
}

// timexNumberRe matches the first number in an lssrc value, which is always
// followed by its unit or by a parenthesised gloss:
//
//	Root dispersion:      0.045678 s
//	Leap indicator:       00 (No leap second today.)
var timexNumberRe = regexp.MustCompile(`[-+]?\d+(?:\.\d+)?`)

// parseLssrcXNTPD parses `lssrc -ls <subsystem>`, the fallback for a daemon
// that refuses mode 6 queries ("restrict default noquery" in /etc/ntp.conf) or
// for a host without ntpq on PATH. SRC asks the daemon over its own channel,
// so it still answers when the query port does not.
//
// The daemon prints a system section of "Label: value" lines, then one block
// per configured peer, and SRC appends the subsystem status table last:
//
//	Program name:    xntpd
//	Version:         4
//	Leap indicator:  00 (No leap second today.)
//	Sys peer:        10.50.1.27
//	Sys stratum:     3
//	Sys precision:   -20
//	Root distance:   0.000000
//	Root dispersion: 0.000000
//	System uptime:   1436883 (sec)
//	Clock stability: 0.000000 (sec)
//	Clock frequency:
//	Peer: 10.50.1.26
//	      flags: (configured)
//	      stratum:  2, version: 4
//	      our mode: client, his mode: server
//	Subsystem         Group            PID          Status
//	xntpd            tcpip            4457442      active
//
// Unlike ntpq, lssrc prints times already in seconds, and reports no offset at
// all — that metric is absent in this mode rather than faked. Several of the
// fields it does print are unfilled placeholders; see setIfPopulated.
func parseLssrcXNTPD(out []byte, subsystem string) ntpSysVars {
	v := newNTPSysVars(timexSourceLssrc)

	var program, version string
	inPeers := false

	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}

		// Status table row: "xntpd  tcpip  4457442  active", or the same with
		// an empty PID column when the subsystem is not running. SRC prints it
		// after the peer list, so the scan cannot simply stop at the peers.
		if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == subsystem {
			v.srcStatus = fields[len(fields)-1]
			continue
		}

		label, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		// "Reference time: ee490466.791e239b  Mon, Sep 7 2026 15:07:42.473"
		// cuts at the wrong colon, which is harmless: the label decides
		// whether the value is read at all.
		label = strings.ToLower(strings.Join(strings.Fields(label), " "))

		// Everything from the first "Peer:" line onwards describes one remote
		// server rather than the local clock, and re-uses label names of its
		// own — "stratum:", "version:", "flags:". Reading those as system
		// variables would publish a peer's stratum as this host's.
		if label == "peer" {
			inPeers = true
			continue
		}
		if inPeers {
			continue
		}

		switch label {
		case "program name":
			program = strings.TrimSpace(value)
			continue
		case "version":
			version = strings.TrimSpace(value)
			continue
		}

		num, ok := firstFloat(value)
		if !ok {
			continue
		}

		switch label {
		case "sys stratum", "system stratum":
			v.set(ntpVarStratum, num)
		case "leap indicator":
			if leap, ok := lssrcLeapIndicator(num); ok {
				v.set(ntpVarLeap, leap)
			}
		case "sys precision", "system precision":
			v.set(ntpVarPrecisionSeconds, math.Exp2(num))
		case "root distance", "root delay":
			// NTP uses "root distance" for rootdelay/2 + rootdisp, which is
			// what maxerror below computes, so this label could mean either
			// quantity. Every 7.x host seen so far leaves it at 0.000000 and
			// therefore publishes neither; if one ever fills it in, check it
			// against ntpq's rootdelay on the same host before trusting it.
			v.setIfPopulated(ntpVarRootDelaySeconds, num)
		case "root dispersion":
			v.setIfPopulated(ntpVarRootDispSeconds, num)
		case "clock frequency":
			v.setIfPopulated(ntpVarFrequencyPPM, num)
		case "clock stability":
			v.setIfPopulated(ntpVarClockWanderPPM, num)
		}
	}

	// ntpq reports the daemon's full version string; lssrc reports the program
	// and the NTP version it implements, which is the closest equivalent it
	// has.
	switch {
	case program != "" && version != "":
		v.version = program + " " + version
	case program != "":
		v.version = program
	default:
		v.version = version
	}

	return v
}

// lssrcLeapIndicator reads the two-digit leap indicator lssrc prints.
//
// ntpq prints the field in decimal (00..03). AIX prints what reads as the two
// bits themselves — "Leap indicator: 00 (No leap second today.)" — and the two
// notations agree on 00 and 01 but diverge above: a clock in the alarm
// condition is 03 under one and 11 under the other. Rather than guess, a value
// above the four the field can hold is read as the bit pair it could only be,
// and anything still out of range afterwards is dropped instead of published
// as a leap indicator the metric's own help text does not define.
func lssrcLeapIndicator(num float64) (float64, bool) {
	switch num {
	case 0, 1, 2, 3:
		return num, true
	case 10: // binary 10: delete a second
		return 2, true
	case 11: // binary 11: alarm, clock not synchronised
		return 3, true
	}
	return 0, false
}

// firstFloat returns the first number appearing in s.
func firstFloat(s string) (float64, bool) {
	m := timexNumberRe.FindString(s)
	if m == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(m, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}
