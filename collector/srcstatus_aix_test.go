// collector/srcstatus_aix_test.go
//go:build aix
// +build aix

package collector

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// AIX 7.3 `lssrc -a`. The Group and PID columns are both optional, and the
// fixture carries one row of every shape the parser has to survive.
const lssrcAllOutput = `Subsystem         Group            PID          Status
 syslogd          ras              123456       active
 sshd             ssh              234567       active
 xntpd            tcpip                         inoperative
 ctrmc                             345678       active
 clcomd                                         inoperative
`

// ----------------------------------------------------------------------------
// Row parsing
// ----------------------------------------------------------------------------

func TestParseSRCRow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		line     string
		wantName string
		wantRow  srcRow
		wantOK   bool
	}{
		{
			"all columns",
			" syslogd          ras              123456       active",
			"syslogd", srcRow{Group: "ras", PID: 123456, Status: "active"}, true,
		},
		{
			"no pid",
			" xntpd            tcpip                         inoperative",
			"xntpd", srcRow{Group: "tcpip", PID: 0, Status: "inoperative"}, true,
		},
		{
			// Without reading the columns from the outside in, the PID here
			// is mistaken for the group and lands in a metric label.
			"no group",
			" ctrmc                             345678       active",
			"ctrmc", srcRow{Group: "", PID: 345678, Status: "active"}, true,
		},
		{
			"neither group nor pid",
			" clcomd                                         inoperative",
			"clcomd", srcRow{Group: "", PID: 0, Status: "inoperative"}, true,
		},
		{
			"header",
			"Subsystem         Group            PID          Status",
			"", srcRow{}, false,
		},
		{"blank", "    ", "", srcRow{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, row, ok := parseSRCRow(tc.line)
			if ok != tc.wantOK || name != tc.wantName || row != tc.wantRow {
				t.Errorf("parseSRCRow(%q) = (%q, %+v, %v), want (%q, %+v, %v)",
					tc.line, name, row, ok, tc.wantName, tc.wantRow, tc.wantOK)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Update semantics
// ----------------------------------------------------------------------------

func TestAIXSRCStatusBulkReportsActive(t *testing.T) {
	c := newTestSRCStatusCollector(t, fakeCommand(t, "lssrc", lssrcAllOutput), srcModeBulk)

	got, err := collectSamples(t, c)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	assertSample(t, got, "node_src_subsystem_up", 1, "subsystem", "sshd", "group", "ssh")
	assertSample(t, got, "node_src_subsystem_pid", 234567, "subsystem", "sshd", "group", "ssh")
	assertSample(t, got, "node_src_subsystem_status_info", 1, "subsystem", "sshd", "status", "active")
	assertSample(t, got, "node_src_subsystem_up", 1, "subsystem", "syslogd", "group", "ras")
}

// TestAIXSRCStatusInoperativeIsPublishedAsZero: SRC answered and said the
// subsystem is not running, which is exactly what 0 is for.
func TestAIXSRCStatusInoperativeIsPublishedAsZero(t *testing.T) {
	c := newTestSRCStatusCollector(t, fakeCommand(t, "lssrc", lssrcAllOutput), srcModeBulk)
	c.subsys = []string{"xntpd"}

	got, err := collectSamples(t, c)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	assertSample(t, got, "node_src_subsystem_up", 0, "subsystem", "xntpd", "group", "tcpip")
	assertSample(t, got, "node_src_subsystem_status_info", 1, "subsystem", "xntpd", "status", "inoperative")
}

// TestAIXSRCStatusUnlistedSubsystemIsPublishedAsZero: the listing succeeded
// and does not mention the subsystem, so SRC does not have it. That is a real
// observation and must still read as down.
func TestAIXSRCStatusUnlistedSubsystemIsPublishedAsZero(t *testing.T) {
	c := newTestSRCStatusCollector(t, fakeCommand(t, "lssrc", lssrcAllOutput), srcModeBulk)
	c.subsys = []string{"netcd"}

	got, err := collectSamples(t, c)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	assertSample(t, got, "node_src_subsystem_up", 0, "subsystem", "netcd")
	assertSample(t, got, "node_src_subsystem_status_info", 1, "subsystem", "netcd", "status", srcStatusAbsent)
}

// TestAIXSRCStatusTimeoutPublishesNothing is the behaviour this collector
// exists to get right: lssrc not coming back says nothing about any
// subsystem, so no series may be published claiming they are down.
func TestAIXSRCStatusTimeoutPublishesNothing(t *testing.T) {
	c := newTestSRCStatusCollector(t, hangingCommand(t, "lssrc"), srcModeBulk)
	c.timeout = 150 * time.Millisecond

	got, err := collectSamples(t, c)
	if err == nil {
		t.Fatal("expected Update to report the lssrc timeout as an error")
	}
	assertNoSamples(t, got, "node_src_subsystem_up")
	assertNoSamples(t, got, "node_src_subsystem_pid")
	assertNoSamples(t, got, "node_src_subsystem_status_info")
	assertSample(t, got, "node_scrape_collector_timeout", 1, "collector", "aix_srcstatus", "reason", "lssrc_a_timeout")
}

// TestAIXSRCStatusEmptyListingPublishesNothing: lssrc exited 0 but printed no
// row. SRC always lists its own subsystems, so this is an unreadable answer,
// not a host on which every subsystem vanished at once.
func TestAIXSRCStatusEmptyListingPublishesNothing(t *testing.T) {
	c := newTestSRCStatusCollector(t, fakeCommand(t, "lssrc", "Subsystem         Group            PID          Status\n"), srcModeBulk)

	got, err := collectSamples(t, c)
	if err == nil {
		t.Fatal("expected Update to report the empty listing as an error")
	}
	assertNoSamples(t, got, "node_src_subsystem_up")
}

// TestAIXSRCStatusUndefinedSubsystemIsPublishedAsZero: in single mode, SRC's
// "not on file" is a definitive answer and must read as down, unlike the
// failures around it.
func TestAIXSRCStatusUndefinedSubsystemIsPublishedAsZero(t *testing.T) {
	script := "#!/bin/sh\n" +
		"echo \"0513-085 The $2 Subsystem is not on file.\" >&2\nexit 1\n"
	c := newTestSRCStatusCollector(t, writeScript(t, "lssrc", script), srcModeSingle)
	c.subsys = []string{"netcd"}

	got, err := collectSamples(t, c)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	assertSample(t, got, "node_src_subsystem_up", 0, "subsystem", "netcd")
	assertSample(t, got, "node_src_subsystem_status_info", 1, "subsystem", "netcd", "status", srcStatusAbsent)
}

// TestAIXSRCStatusPartialFailureKeepsTheRest: one subsystem lssrc could not
// answer for drops out of the scrape; the ones it did answer for are still
// published.
func TestAIXSRCStatusPartialFailureKeepsTheRest(t *testing.T) {
	script := "#!/bin/sh\n" +
		"if [ \"$2\" = netcd ]; then\n" +
		"  echo '0513-004 The Subsystem Resource Controller is not active.' >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"cat <<'FIXTURE'\n" +
		"Subsystem         Group            PID          Status\n" +
		" sshd             ssh              234567       active\n" +
		"FIXTURE\n"
	c := newTestSRCStatusCollector(t, writeScript(t, "lssrc", script), srcModeSingle)
	c.subsys = []string{"netcd", "sshd"}

	got, err := collectSamples(t, c)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	assertSample(t, got, "node_src_subsystem_up", 1, "subsystem", "sshd", "group", "ssh")
	assertNoSamples(t, got, "node_src_subsystem_up", "subsystem", "netcd")
	assertNoSamples(t, got, "node_src_subsystem_pid", "subsystem", "netcd")
}

// TestAIXSRCStatusGroupLabelSurvivesSubsystemLoss: a subsystem that
// disappears from lssrc keeps the group it was last seen in, so the alert
// watching node_src_subsystem_up{group="ssh"} sees it go to 0 instead of
// losing the series to a second one labelled "unknown".
func TestAIXSRCStatusGroupLabelSurvivesSubsystemLoss(t *testing.T) {
	c := newTestSRCStatusCollector(t, fakeCommand(t, "lssrc", lssrcAllOutput), srcModeBulk)
	c.subsys = []string{"sshd"}

	if _, err := collectSamples(t, c); err != nil {
		t.Fatalf("first Update: %v", err)
	}

	// sshd is gone from SRC on the next scrape.
	c.lssrcPath = fakeCommand(t, "lssrc",
		"Subsystem         Group            PID          Status\n syslogd          ras              123456       active\n")

	got, err := collectSamples(t, c)
	if err != nil {
		t.Fatalf("second Update: %v", err)
	}
	assertSample(t, got, "node_src_subsystem_up", 0, "subsystem", "sshd", "group", "ssh")
}

// ----------------------------------------------------------------------------
// Harness
// ----------------------------------------------------------------------------

func newTestSRCStatusCollector(t *testing.T, lssrc, mode string) *aixSRCStatusCollector {
	t.Helper()
	c, err := NewAIXSRCStatusCollector(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// The flag pointers hold their defaults only after kingpin has parsed, so
	// the test sets the fields the flags would have filled in.
	sc := c.(*aixSRCStatusCollector)
	sc.lssrcPath = lssrc
	sc.mode = mode
	sc.subsys = []string{"sshd", "syslogd"}
	sc.timeout = 5 * time.Second
	return sc
}

// writeScript drops an executable /bin/sh script into a temp dir.
func writeScript(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestBudgetedCommandContextReservesWaitDelay pins the headroom that keeps a
// timed-out command inside the collector's budget: killing the command is not
// the end of it, Cmd.Wait still drains its pipes for up to cmdWaitDelay, and a
// scrape that overruns loses every collector's metrics rather than just this
// collector's.
func TestBudgetedCommandContextReservesWaitDelay(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, tc := range []struct {
		name   string
		budget time.Duration
		want   time.Duration
		expect time.Duration
	}{
		{"budget is the binding constraint", 3 * time.Second, 8 * time.Second, 1 * time.Second},
		{"per-command timeout is the binding constraint", 30 * time.Second, 5 * time.Second, 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guard := NewTimeoutGuard("test", logger, tc.budget)
			ctx, cancel := budgetedCommandContext(context.Background(), guard, tc.want)
			defer cancel()

			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("expected the command context to carry a deadline")
			}
			if got := time.Until(deadline); got > tc.expect || got < tc.expect-time.Second {
				t.Errorf("command deadline in %s, want about %s", got, tc.expect)
			}
		})
	}
}
