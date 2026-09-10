// collector/process_aix_test.go
//go:build aix
// +build aix

package collector

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// AIX 7.3 `ps -eo pid=,args=`: no header, PID right-aligned, executable second.
const psFormatOutput = `      1 /etc/init
 123456 /usr/sbin/cron
 234567 /usr/lib/errdemon
 345678 /usr/bin/ksh -c /usr/sbin/cron
 456789 sshd: root@pts/0
`

// The same host through `ps -ef`, including both STIME forms.
const psEFOutput = `    UID    PID   PPID   C    STIME    TTY  TIME CMD
   root      1      0   0   Sep 01      -  0:12 /etc/init
   root 123456      1   0   Sep 01      -  0:00 /usr/sbin/cron
   root 234567      1   0 09:12:33      -  0:03 /usr/lib/errdemon
   root 345678      1   0 09:14:01  pts/0  0:00 /usr/bin/ksh -c /usr/sbin/cron
`

// ----------------------------------------------------------------------------
// Row parsing
// ----------------------------------------------------------------------------

func TestParsePSFormatRow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		line    string
		wantPID int
		wantCmd string
		wantOK  bool
	}{
		{"plain", " 123456 /usr/sbin/cron", 123456, "/usr/sbin/cron", true},
		{"with args", " 345678 /usr/bin/ksh -c /usr/sbin/cron", 345678, "/usr/bin/ksh", true},
		{"pid only", " 123456", 0, "", false},
		{"header text", "    PID COMMAND", 0, "", false},
		{"blank", "   ", 0, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pid, cmd, ok := parsePSFormatRow(tc.line)
			if ok != tc.wantOK || pid != tc.wantPID || cmd != tc.wantCmd {
				t.Errorf("parsePSFormatRow(%q) = (%d, %q, %v), want (%d, %q, %v)",
					tc.line, pid, cmd, ok, tc.wantPID, tc.wantCmd, tc.wantOK)
			}
		})
	}
}

// TestParsePSEFRowHandlesSTIMEShift pins the column drift that makes ps -ef
// awkward: a two-token STIME pushes CMD one field to the right.
func TestParsePSEFRowHandlesSTIMEShift(t *testing.T) {
	for _, tc := range []struct {
		name    string
		line    string
		wantPID int
		wantCmd string
		wantOK  bool
	}{
		{
			"started today",
			"   root 234567      1   0 09:12:33      -  0:03 /usr/lib/errdemon",
			234567, "/usr/lib/errdemon", true,
		},
		{
			"started earlier",
			"   root 123456      1   0   Sep 01      -  0:00 /usr/sbin/cron",
			123456, "/usr/sbin/cron", true,
		},
		{
			"header",
			"    UID    PID   PPID   C    STIME    TTY  TIME CMD",
			0, "", false,
		},
		{
			"truncated",
			"   root 123456      1   0   Sep 01      -",
			0, "", false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pid, cmd, ok := parsePSEFRow(tc.line)
			if ok != tc.wantOK || pid != tc.wantPID || cmd != tc.wantCmd {
				t.Errorf("parsePSEFRow(%q) = (%d, %q, %v), want (%d, %q, %v)",
					tc.line, pid, cmd, ok, tc.wantPID, tc.wantCmd, tc.wantOK)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Update semantics
// ----------------------------------------------------------------------------

// TestAIXProcessUpReportsRunningProcesses is the ordinary path: ps answered,
// both targets are in the table.
func TestAIXProcessUpReportsRunningProcesses(t *testing.T) {
	c := newTestProcessCollector(t, fakeCommand(t, "ps", psFormatOutput))

	got, err := collectSamples(t, c)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	assertSample(t, got, "node_process_up", 1, "process", "/usr/sbin/cron")
	assertSample(t, got, "node_process_pid", 123456, "process", "/usr/sbin/cron")
	assertSample(t, got, "node_process_up", 1, "process", "/usr/lib/errdemon")
	assertSample(t, got, "node_process_pid", 234567, "process", "/usr/lib/errdemon")
}

// TestAIXProcessDownIsPublishedAsZero is the other half of the contract: when
// ps answers and the process is genuinely not there, 0 must be published. The
// absence of a metric has to mean "unknown", so a real outage cannot rely on
// it.
func TestAIXProcessDownIsPublishedAsZero(t *testing.T) {
	c := newTestProcessCollector(t, fakeCommand(t, "ps", " 1 /etc/init\n 4242 /usr/sbin/syslogd\n"))

	got, err := collectSamples(t, c)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	for _, target := range []string{"/usr/sbin/cron", "/usr/lib/errdemon"} {
		assertSample(t, got, "node_process_up", 0, "process", target)
		assertSample(t, got, "node_process_pid", 0, "process", target)
	}
}

// TestAIXProcessArgumentsDoNotCountAsRunning guards the false positive in
// matching every token of the command line: `ksh -c /usr/sbin/cron` must not
// make /usr/sbin/cron look like a running process, nor lend it its PID.
func TestAIXProcessArgumentsDoNotCountAsRunning(t *testing.T) {
	c := newTestProcessCollector(t, fakeCommand(t, "ps", " 1 /etc/init\n 345678 /usr/bin/ksh -c /usr/sbin/cron\n"))

	got, err := collectSamples(t, c)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	assertSample(t, got, "node_process_up", 0, "process", "/usr/sbin/cron")
	assertSample(t, got, "node_process_pid", 0, "process", "/usr/sbin/cron")
}

// TestAIXProcessTimeoutPublishesNothing is the behaviour this collector exists
// to get right: a ps that does not come back within the timeout says nothing
// about the monitored processes, so no up/pid series may be published. Only
// the scrape-level timeout metric is emitted.
func TestAIXProcessTimeoutPublishesNothing(t *testing.T) {
	c := newTestProcessCollector(t, hangingCommand(t, "ps"))
	c.timeout = 150 * time.Millisecond

	got, err := collectSamples(t, c)
	if err == nil {
		t.Fatal("expected Update to report the ps timeout as an error")
	}
	assertNoSamples(t, got, "node_process_up")
	assertNoSamples(t, got, "node_process_pid")
	assertSample(t, got, "node_scrape_collector_timeout", 1, "collector", "aix_process")
}

// TestAIXProcessCommandFailurePublishesNothing covers the non-timeout
// failures — ps missing, exec denied — which are equally uninformative.
func TestAIXProcessCommandFailurePublishesNothing(t *testing.T) {
	c := newTestProcessCollector(t, filepath.Join(t.TempDir(), "no-such-ps"))

	got, err := collectSamples(t, c)
	if err == nil {
		t.Fatal("expected Update to report the ps failure as an error")
	}
	assertNoSamples(t, got, "node_process_up")
	assertNoSamples(t, got, "node_process_pid")
}

// TestAIXProcessEmptyTablePublishesNothing covers a ps that exits 0 but prints
// nothing readable. A live AIX system always has processes, so an empty table
// is a broken reading, not an empty host.
func TestAIXProcessEmptyTablePublishesNothing(t *testing.T) {
	c := newTestProcessCollector(t, fakeCommand(t, "ps", ""))

	got, err := collectSamples(t, c)
	if err == nil {
		t.Fatal("expected Update to report the unreadable process table as an error")
	}
	assertNoSamples(t, got, "node_process_up")
}

// TestAIXProcessFallsBackToPSEF covers a ps that rejects -o: the collector
// must retry with -ef and parse the wider table rather than report the host's
// processes as down.
func TestAIXProcessFallsBackToPSEF(t *testing.T) {
	c := newTestProcessCollector(t, psRejectingFormat(t, psEFOutput))

	got, err := collectSamples(t, c)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	assertSample(t, got, "node_process_up", 1, "process", "/usr/sbin/cron")
	assertSample(t, got, "node_process_pid", 123456, "process", "/usr/sbin/cron")
	assertSample(t, got, "node_process_up", 1, "process", "/usr/lib/errdemon")
	assertSample(t, got, "node_process_pid", 234567, "process", "/usr/lib/errdemon")
}

// TestAIXProcessKeepsLowestPID pins the tie-break for a process with several
// instances, so the reported PID does not flap between them.
func TestAIXProcessKeepsLowestPID(t *testing.T) {
	c := newTestProcessCollector(t, fakeCommand(t, "ps",
		" 990 /etc/init\n 5000 /usr/sbin/cron\n 3000 /usr/sbin/cron\n 7000 /usr/sbin/cron\n"))

	got, err := collectSamples(t, c)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	assertSample(t, got, "node_process_pid", 3000, "process", "/usr/sbin/cron")
}

// ----------------------------------------------------------------------------
// Harness
// ----------------------------------------------------------------------------

func newTestProcessCollector(t *testing.T, psPath string) *aixProcessCollector {
	t.Helper()
	c, err := NewAIXProcessCollector(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// The flag pointers hold their defaults only after kingpin has parsed, so
	// the test sets the fields the flags would have filled in.
	pc := c.(*aixProcessCollector)
	pc.psPath = psPath
	pc.targets = []string{"/usr/sbin/cron", "/usr/lib/errdemon"}
	pc.timeout = 5 * time.Second
	return pc
}

// hangingCommand writes a script that never produces output, standing in for a
// command wedged on an unresponsive host.
func hangingCommand(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// psRejectingFormat writes a ps that refuses -o, as an older build would, and
// prints the given -ef table otherwise.
func psRejectingFormat(t *testing.T, efOutput string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ps")
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"-eo) echo 'ps: illegal option -- o' >&2; exit 1 ;;\n" +
		"esac\n" +
		"cat <<'FIXTURE'\n" + efOutput + "FIXTURE\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// sample is one emitted metric, flattened for assertions.
type sample struct {
	name   string
	labels map[string]string
	value  float64
}

// collectSamples runs one scrape and returns everything it emitted along with
// the collector's error, so a test can assert on both.
func collectSamples(t *testing.T, c Collector) ([]sample, error) {
	t.Helper()
	ch := make(chan prometheus.Metric, 256)
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.Update(ch)
		close(ch)
	}()

	var got []sample
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("writing metric: %v", err)
		}
		s := sample{name: fqNameOf(m.Desc().String()), labels: map[string]string{}}
		switch {
		case pb.GetGauge() != nil:
			s.value = pb.GetGauge().GetValue()
		case pb.GetCounter() != nil:
			s.value = pb.GetCounter().GetValue()
		}
		for _, lp := range pb.GetLabel() {
			s.labels[lp.GetName()] = lp.GetValue()
		}
		got = append(got, s)
	}
	return got, <-errCh
}

// matches reports whether s carries every label in the name=value pairs.
func (s sample) matches(name string, labelPairs ...string) bool {
	if s.name != name {
		return false
	}
	for i := 0; i+1 < len(labelPairs); i += 2 {
		if s.labels[labelPairs[i]] != labelPairs[i+1] {
			return false
		}
	}
	return true
}

func assertSample(t *testing.T, got []sample, name string, want float64, labelPairs ...string) {
	t.Helper()
	for _, s := range got {
		if s.matches(name, labelPairs...) {
			if s.value != want {
				t.Errorf("%s%v = %v, want %v", name, labelPairs, s.value, want)
			}
			return
		}
	}
	t.Errorf("%s%v was not emitted", name, labelPairs)
}

func assertNoSamples(t *testing.T, got []sample, name string, labelPairs ...string) {
	t.Helper()
	for _, s := range got {
		if s.matches(name, labelPairs...) {
			t.Errorf("%s%v was emitted as %v; it must be absent when the state is unknown",
				name, labelPairs, s.value)
		}
	}
}
