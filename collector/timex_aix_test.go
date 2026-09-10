// collector/timex_aix_test.go
//go:build aix
// +build aix

package collector

import (
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Captured from `ntpq -c "rv 0"` on a synchronised AIX host running the NTPv4
// daemon, which is what /usr/sbin/ntpq is on AIX 7.x.
const ntpqV4Output = `associd=0 status=0615 leap_none, sync_ntp, 1 event, clock_sync,
version="ntpd 4.2.8p17@1.4004-o Tue Aug 12 14:32:16 UTC 2025 (1)",
processor="00FBE6AA4C00", system="AIX/3", leap=00, stratum=3,
precision=-20, rootdelay=25.254, rootdisp=29.586, refid=10.50.1.27,
reftime=ee490466.791e239b  Mon, Sep  7 2026 15:07:42.473,
clock=ee4905c3.f56079bb  Mon, Sep  7 2026 15:13:31.958, peer=43871,
tc=10, mintc=3, offset=+0.444837, frequency=-6.958, sys_jitter=1.330295,
clk_jitter=0.197, clk_wander=0.000
`

// An AIX 7.1/7.2 host still running the NTPv3 daemon reports the same
// quantities under the older names.
const ntpqV3Output = `status=0644 leap_none, sync_ntp, 4 events, event_peer/strat_chg,
version="ntpd 3-5.93e Mon Sep 20 15:47:11 GMT 1999 (1)",
processor="powerpc", system="AIX/7.1", leap=00, stratum=2,
precision=-17, rootdelay=39.51, rootdispersion=47.62, peer=24274,
refid=10.70.20.1, poll=10, state=4, phase=0.203, freq=-13.185,
error=0.024, jitter=0.512, stability=0.031
`

// Captured from `lssrc -ls xntpd` on the same host, at the same moment as
// ntpqV4Output above. The daemon is synchronised to a stratum-2 server and
// ntpq reports a 25ms root delay, yet SRC prints Root distance, Root
// dispersion and Clock stability as 0.000000 and Clock frequency as nothing at
// all: AIX's status handler does not fill these in.
const lssrcActiveOutput = `Program name:    xntpd
Version:         4
Leap indicator:  00 (No leap second today.)
Sys peer:        10.50.1.27
Sys stratum:     3
Sys precision:   -20
Debug/Tracing:   DISABLED
Root distance:   0.000000
Root dispersion: 0.000000
Reference ID:    10.50.1.27
Reference time:  ee490466.791e239b  Mon, Sep  7 2026 15:07:42.473
Broadcast delay: 0.000000 (sec)
Auth delay:      +0.000000 (sec)
System flags:    auth monitor filegen
System uptime:   1436883 (sec)
Clock stability: 0.000000 (sec)
Clock frequency:
Peer: 10.50.1.26
      flags: (configured)
      stratum:  2, version: 4
      our mode: client, his mode: server
Peer: 10.50.1.27
      flags: (configured)
      stratum:  2, version: 4
      our mode: client, his mode: server
Subsystem         Group            PID          Status
xntpd            tcpip            4457442      active
`

// A host whose SRC handler does populate the clock fields. No AIX 7.x host has
// been seen doing so, but the values are read when they are there.
const lssrcPopulatedOutput = `Program name:    xntpd
Version:         4
Leap indicator:  00 (No leap second today.)
Sys stratum:     3
Sys precision:   -20
Root distance:   0.012345 s
Root dispersion: 0.045678 s
Clock stability: 0.010 ppm
Clock frequency: -12.345 ppm
Subsystem         Group            PID          Status
 xntpd            tcpip            5636300      active
`

const lssrcInoperativeOutput = `Subsystem         Group            PID          Status
 xntpd            tcpip                         inoperative
`

func closeEnough(got, want float64) bool {
	return math.Abs(got-want) <= 1e-12+math.Abs(want)*1e-9
}

func checkVar(t *testing.T, v ntpSysVars, key string, want float64) {
	t.Helper()
	got, ok := v.get(key)
	if !ok {
		t.Errorf("%s: not parsed, want %v", key, want)
		return
	}
	if !closeEnough(got, want) {
		t.Errorf("%s: got %v, want %v", key, got, want)
	}
}

func TestParseNTPQSysVarsV4(t *testing.T) {
	v := parseNTPQSysVars([]byte(ntpqV4Output))

	if v.empty() {
		t.Fatal("parsed no system variables")
	}
	if want := "ntpd 4.2.8p17@1.4004-o Tue Aug 12 14:32:16 UTC 2025 (1)"; v.version != want {
		t.Errorf("version: got %q, want %q", v.version, want)
	}

	// ntpq reports times in milliseconds and frequencies in ppm. The offset is
	// printed with an explicit "+", which has to survive the number parse.
	checkVar(t, v, ntpVarOffsetSeconds, 0.444837/1000)
	checkVar(t, v, ntpVarFrequencyPPM, -6.958)
	checkVar(t, v, ntpVarRootDelaySeconds, 25.254/1000)
	checkVar(t, v, ntpVarRootDispSeconds, 29.586/1000)
	checkVar(t, v, ntpVarSysJitterSeconds, 1.330295/1000)
	checkVar(t, v, ntpVarClkJitterSeconds, 0.197/1000)
	// A measured zero from ntpq is a real reading and is kept, unlike the
	// unfilled zeros lssrc prints.
	checkVar(t, v, ntpVarClockWanderPPM, 0)
	checkVar(t, v, ntpVarPrecisionSeconds, math.Exp2(-20))
	checkVar(t, v, ntpVarStratum, 3)
	checkVar(t, v, ntpVarLeap, 0)
	checkVar(t, v, ntpVarLoopTimeConstant, 10)

	if _, ok := v.get(ntpVarTAISeconds); ok {
		t.Error("tai_offset_seconds present though the daemon reported no tai")
	}

	sync, ok := v.syncStatus()
	if !ok || sync != 1 {
		t.Errorf("syncStatus: got (%v, %v), want (1, true)", sync, ok)
	}
}

// The date fields that spill out of reftime/clock, and the spaces inside the
// quoted version string, must not be mistaken for values.
func TestParseNTPQSysVarsIgnoresNonVariables(t *testing.T) {
	v := parseNTPQSysVars([]byte(ntpqV4Output))

	for _, key := range []string{
		"Mon", "Sep", "reftime", "clock", "peer", "mintc", "refid",
		"associd", "status", "processor", "system",
	} {
		if _, ok := v.vals[key]; ok {
			t.Errorf("%q was stored as a variable", key)
		}
	}
	// tc=10 follows the reftime date on its line; the date must not have
	// swallowed it.
	checkVar(t, v, ntpVarLoopTimeConstant, 10)
}

func TestParseNTPQSysVarsV3Aliases(t *testing.T) {
	v := parseNTPQSysVars([]byte(ntpqV3Output))

	checkVar(t, v, ntpVarOffsetSeconds, 0.203/1000) // phase
	checkVar(t, v, ntpVarFrequencyPPM, -13.185)     // freq
	checkVar(t, v, ntpVarRootDispSeconds, 47.62/1000)
	checkVar(t, v, ntpVarClkJitterSeconds, 0.024/1000) // error
	checkVar(t, v, ntpVarSysJitterSeconds, 0.512/1000) // jitter
	checkVar(t, v, ntpVarClockWanderPPM, 0.031)        // stability
	checkVar(t, v, ntpVarStratum, 2)

	sync, ok := v.syncStatus()
	if !ok || sync != 1 {
		t.Errorf("syncStatus: got (%v, %v), want (1, true)", sync, ok)
	}
}

func TestParseNTPQSysVarsUnsynchronized(t *testing.T) {
	const out = `associd=0 status=c011 leap_alarm, sync_unspec, 1 event, clock_unspec,
leap=03, stratum=16, precision=-20, rootdelay=0.000, rootdisp=1.234,
refid=INIT, offset=0.000000, frequency=0.000, sys_jitter=0.000000
`
	v := parseNTPQSysVars([]byte(out))

	checkVar(t, v, ntpVarLeap, leapAlarm)
	checkVar(t, v, ntpVarStratum, unsyncStratum)

	sync, ok := v.syncStatus()
	if !ok || sync != 0 {
		t.Errorf("syncStatus: got (%v, %v), want (0, true)", sync, ok)
	}
}

// A daemon terse enough to report no leap= variable still reports the status
// word, whose top two bits carry the same indicator.
func TestParseNTPQSysVarsLeapFromStatusWord(t *testing.T) {
	v := parseNTPQSysVars([]byte("associd=0 status=c011 leap_alarm, sync_unspec,\nstratum=16\n"))

	checkVar(t, v, ntpVarLeap, leapAlarm)

	sync, ok := v.syncStatus()
	if !ok || sync != 0 {
		t.Errorf("syncStatus: got (%v, %v), want (0, true)", sync, ok)
	}
}

// ntpq exits 0 after printing a refusal, so a reading with nothing in it is
// what tells the collector to fall back to lssrc.
func TestParseNTPQSysVarsRefused(t *testing.T) {
	for _, out := range []string{
		"***Request timed out\n",
		"ntpq: read: Connection refused\n",
		"",
	} {
		if v := parseNTPQSysVars([]byte(out)); !v.empty() {
			t.Errorf("%q: got %+v, want an empty reading", out, v.vals)
		}
	}
}

func TestParseLssrcActive(t *testing.T) {
	v := parseLssrcXNTPD([]byte(lssrcActiveOutput), "xntpd")

	if v.srcStatus != "active" {
		t.Errorf("srcStatus: got %q, want %q", v.srcStatus, "active")
	}
	if want := "xntpd 4"; v.version != want {
		t.Errorf("version: got %q, want %q", v.version, want)
	}
	checkVar(t, v, ntpVarPrecisionSeconds, math.Exp2(-20))
	checkVar(t, v, ntpVarLeap, 0)

	// The peer blocks carry "stratum:  2" lines of their own. Reading those as
	// system variables would publish a remote server's stratum as this host's.
	checkVar(t, v, ntpVarStratum, 3)

	// The fields AIX leaves at 0.000000 must be absent, not published: a root
	// delay and dispersion of zero would say this clock has no error at all.
	for _, key := range []string{
		ntpVarRootDelaySeconds,
		ntpVarRootDispSeconds,
		ntpVarFrequencyPPM,
		ntpVarClockWanderPPM,
	} {
		if got, ok := v.get(key); ok {
			t.Errorf("%s: got %v, want absent — lssrc printed an unfilled 0.000000", key, got)
		}
	}

	// lssrc reports no offset; it must be absent rather than zero.
	if _, ok := v.get(ntpVarOffsetSeconds); ok {
		t.Error("offset_seconds present though lssrc does not report one")
	}

	sync, ok := v.syncStatus()
	if !ok || sync != 1 {
		t.Errorf("syncStatus: got (%v, %v), want (1, true)", sync, ok)
	}
}

// The zero-dropping must not throw away a host that does report these fields.
func TestParseLssrcPopulated(t *testing.T) {
	v := parseLssrcXNTPD([]byte(lssrcPopulatedOutput), "xntpd")

	// lssrc prints times in seconds already.
	checkVar(t, v, ntpVarRootDelaySeconds, 0.012345)
	checkVar(t, v, ntpVarRootDispSeconds, 0.045678)
	checkVar(t, v, ntpVarFrequencyPPM, -12.345)
	checkVar(t, v, ntpVarClockWanderPPM, 0.010)
	checkVar(t, v, ntpVarStratum, 3)
}

// AIX prints the leap indicator as two digits that read as bits, where ntpq
// prints the same field in decimal. The two agree below 02 and diverge above,
// and a value that is neither is not published at all.
func TestLssrcLeapIndicator(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want float64
		ok   bool
	}{
		{0, 0, true},
		{1, 1, true},
		{2, 2, true},
		{3, 3, true},
		{10, 2, true},
		{11, 3, true},
		{4, 0, false},
		{99, 0, false},
	} {
		got, ok := lssrcLeapIndicator(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("lssrcLeapIndicator(%v) = (%v, %v), want (%v, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// A daemon that is not running is a definitive "not synchronised", not a
// missing reading.
func TestParseLssrcInoperative(t *testing.T) {
	v := parseLssrcXNTPD([]byte(lssrcInoperativeOutput), "xntpd")

	if v.empty() {
		t.Fatal("reading is empty, so the collector would report no data")
	}
	if v.srcStatus != "inoperative" {
		t.Errorf("srcStatus: got %q, want %q", v.srcStatus, "inoperative")
	}
	sync, ok := v.syncStatus()
	if !ok || sync != 0 {
		t.Errorf("syncStatus: got (%v, %v), want (0, true)", sync, ok)
	}
}

// Nothing usable in, nothing published out.
func TestParseLssrcUnknownSubsystem(t *testing.T) {
	const out = `0513-085 The xntpd Subsystem is not on file.
`
	if v := parseLssrcXNTPD([]byte(out), "xntpd"); !v.empty() {
		t.Errorf("got %+v/%q, want an empty reading", v.vals, v.srcStatus)
	}
}

// An incomplete reading must not be published as "unsynchronised".
func TestSyncStatusUnknown(t *testing.T) {
	v := newNTPSysVars(timexSourceNTPQ)
	v.set(ntpVarOffsetSeconds, 0.001)

	if sync, ok := v.syncStatus(); ok {
		t.Errorf("syncStatus: got (%v, true), want unknown", sync)
	}
}

// ----------------------------------------------------------------------------
// End to end: Update against stand-in ntpq/lssrc commands
// ----------------------------------------------------------------------------

// fakeCommand writes a script that prints out on stdout, standing in for ntpq
// or lssrc.
func fakeCommand(t *testing.T, name, out string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\ncat <<'FIXTURE'\n"+out+"FIXTURE\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestTimexCollector(t *testing.T, ntpq, lssrc string) *aixTimexCollector {
	t.Helper()
	c, err := NewAIXTimexCollector(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// The flag pointers hold their defaults only after kingpin has parsed, so
	// the test sets the fields the flags would have filled in.
	tc := c.(*aixTimexCollector)
	tc.source = timexSourceAuto
	tc.ntpqPath, tc.ntpqHost = ntpq, "127.0.0.1"
	tc.lssrcPath, tc.subsystem = lssrc, "xntpd"
	tc.timeout = 5 * time.Second
	return tc
}

// collectTimex runs one scrape and returns the emitted samples by metric name.
func collectTimex(t *testing.T, c Collector) (map[string]float64, map[string][]string) {
	t.Helper()
	ch := make(chan prometheus.Metric, 64)
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.Update(ch)
		close(ch)
	}()

	values := map[string]float64{}
	labels := map[string][]string{}
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("writing metric: %v", err)
		}
		name := fqNameOf(m.Desc().String())
		switch {
		case pb.GetGauge() != nil:
			values[name] = pb.GetGauge().GetValue()
		case pb.GetCounter() != nil:
			values[name] = pb.GetCounter().GetValue()
		}
		for _, lp := range pb.GetLabel() {
			labels[name] = append(labels[name], lp.GetName()+"="+lp.GetValue())
		}
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Update: %v", err)
	}
	return values, labels
}

// fqNameOf pulls the metric name out of Desc.String(), which renders as
// `Desc{fqName: "node_timex_offset_seconds", help: ...}`.
func fqNameOf(desc string) string {
	_, rest, ok := strings.Cut(desc, `fqName: "`)
	if !ok {
		return desc
	}
	name, _, _ := strings.Cut(rest, `"`)
	return name
}

func TestAIXTimexUpdateFromNTPQ(t *testing.T) {
	c := newTestTimexCollector(t, fakeCommand(t, "ntpq", ntpqV4Output), "/nonexistent/lssrc")

	values, labels := collectTimex(t, c)

	want := map[string]float64{
		"node_timex_sync_status":                1,
		"node_timex_offset_seconds":             0.444837 / 1000,
		"node_timex_frequency_adjustment_ratio": 1 + -6.958/1e6,
		"node_timex_maxerror_seconds":           (25.254/1000)/2 + 29.586/1000,
		"node_timex_estimated_error_seconds":    0.197 / 1000,
		"node_timex_loop_time_constant":         10,
		"node_timex_stratum":                    3,
		"node_timex_leap":                       0,
		"node_timex_root_delay_seconds":         25.254 / 1000,
		"node_timex_root_dispersion_seconds":    29.586 / 1000,
		"node_timex_sys_jitter_seconds":         1.330295 / 1000,
		"node_timex_clock_wander_ppm":           0,
		"node_timex_precision_seconds":          math.Exp2(-20),
		"node_timex_source_info":                1,
	}
	for name, w := range want {
		got, ok := values[name]
		if !ok {
			t.Errorf("%s: not emitted", name)
			continue
		}
		if !closeEnough(got, w) {
			t.Errorf("%s: got %v, want %v", name, got, w)
		}
	}
	// The kernel-only series of timex.go must stay absent, not be faked.
	for _, name := range []string{
		"node_timex_tick_seconds",
		"node_timex_status",
		"node_timex_pps_frequency_hertz",
		"node_timex_tai_offset_seconds",
	} {
		if _, ok := values[name]; ok {
			t.Errorf("%s emitted, but this source cannot supply it", name)
		}
	}
	if got := labels["node_timex_source_info"]; len(got) != 2 || got[0] != "source=ntpq" {
		t.Errorf("source_info labels: got %v", got)
	}
}

// A daemon refusing mode 6 queries must not fail the scrape: SRC still answers.
func TestAIXTimexUpdateFallsBackToLssrc(t *testing.T) {
	c := newTestTimexCollector(t,
		fakeCommand(t, "ntpq", "***Request timed out\n"),
		fakeCommand(t, "lssrc", lssrcActiveOutput))

	values, labels := collectTimex(t, c)

	if got, want := values["node_timex_sync_status"], 1.0; got != want {
		t.Errorf("sync_status: got %v, want %v", got, want)
	}
	if got, want := values["node_timex_stratum"], 3.0; got != want {
		t.Errorf("stratum: got %v, want %v", got, want)
	}
	// What this source cannot supply stays absent. maxerror is the one that
	// matters: derived from the two root fields AIX leaves at zero, it would
	// otherwise be published as 0 — a clock with no error at all.
	for _, name := range []string{
		"node_timex_offset_seconds",
		"node_timex_maxerror_seconds",
		"node_timex_root_delay_seconds",
		"node_timex_root_dispersion_seconds",
		"node_timex_frequency_adjustment_ratio",
		"node_timex_clock_wander_ppm",
		"node_timex_estimated_error_seconds",
	} {
		if got, ok := values[name]; ok {
			t.Errorf("%s emitted as %v, but lssrc did not report it", name, got)
		}
	}
	if got := labels["node_timex_source_info"]; len(got) == 0 || got[0] != "source=lssrc" {
		t.Errorf("source_info labels: got %v", got)
	}
}

func TestAIXTimexUpdateDaemonDown(t *testing.T) {
	c := newTestTimexCollector(t,
		fakeCommand(t, "ntpq", "ntpq: read: Connection refused\n"),
		fakeCommand(t, "lssrc", lssrcInoperativeOutput))

	values, _ := collectTimex(t, c)

	if got, want := values["node_timex_sync_status"], 0.0; got != want {
		t.Errorf("sync_status: got %v, want %v", got, want)
	}
}

// With no usable source the scrape must fail visibly rather than publish a
// clock that looks fine.
func TestAIXTimexUpdateBothSourcesFail(t *testing.T) {
	c := newTestTimexCollector(t, "/nonexistent/ntpq", "/nonexistent/lssrc")

	ch := make(chan prometheus.Metric, 16)
	err := c.Update(ch)
	close(ch)
	if err == nil {
		t.Fatal("Update succeeded with no working source")
	}
	for m := range ch {
		t.Errorf("metric emitted despite total failure: %v", m.Desc())
	}
}
