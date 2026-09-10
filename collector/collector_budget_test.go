// Copyright 2015 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package collector

import (
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// wedgedCollector models an AIX collector blocked on an uninterruptible command:
// it ignores every cancellation signal and returns only when released.
type wedgedCollector struct {
	release chan struct{}
	started chan struct{}
	once    sync.Once
}

func newWedgedCollector() *wedgedCollector {
	return &wedgedCollector{
		release: make(chan struct{}),
		started: make(chan struct{}, 16),
	}
}

func (w *wedgedCollector) Update(ch chan<- prometheus.Metric) error {
	w.started <- struct{}{}
	<-w.release
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc("wedged_metric", "help", nil, nil),
		prometheus.GaugeValue, 1,
	)
	return nil
}

func (w *wedgedCollector) Release() { w.once.Do(func() { close(w.release) }) }

// fastCollector stands in for the healthy collectors on the same scrape.
type fastCollector struct{ name string }

func (f fastCollector) Update(ch chan<- prometheus.Metric) error {
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(f.name+"_metric", "help", nil, nil),
		prometheus.GaugeValue, 42,
	)
	return nil
}

// collect drains NodeCollector.Collect and returns the rendered metrics.
func collect(t *testing.T, n *NodeCollector) []string {
	t.Helper()
	ch := make(chan prometheus.Metric, 4096)
	done := make(chan struct{})
	var got []string
	go func() {
		defer close(done)
		for m := range ch {
			got = append(got, m.Desc().String())
		}
	}()
	n.Collect(ch)
	close(ch)
	<-done
	return got
}

func hasMetric(got []string, fqName string) bool {
	for _, g := range got {
		if strings.Contains(g, `fqName: "`+fqName+`"`) {
			return true
		}
	}
	return false
}

// TestWedgedCollectorDoesNotBlockOthers is the property that matters in
// production: when one collector blocks on an uninterruptible command, every
// other collector's metrics must still reach the scrape, and Collect must
// return within the budget rather than running past scrape_timeout.
func TestWedgedCollectorDoesNotBlockOthers(t *testing.T) {
	old := *maxCollectorDuration
	budget := 150 * time.Millisecond
	*maxCollectorDuration = budget
	defer func() { *maxCollectorDuration = old }()

	wedged := newWedgedCollector()
	defer wedged.Release()

	n := &NodeCollector{
		logger: testLogger(),
		Collectors: map[string]Collector{
			"wedged": wedged,
			"fast_a": fastCollector{name: "fast_a"},
			"fast_b": fastCollector{name: "fast_b"},
		},
	}

	start := time.Now()
	got := collect(t, n)
	elapsed := time.Since(start)

	if elapsed > budget*4 {
		t.Fatalf("Collect blocked for %s; the wedged collector was not abandoned", elapsed)
	}
	for _, want := range []string{"fast_a_metric", "fast_b_metric"} {
		if !hasMetric(got, want) {
			t.Errorf("healthy collector metric %q missing; a wedged collector cost the whole scrape", want)
		}
	}
	if !hasMetric(got, "node_scrape_collector_abandoned") {
		t.Error("expected node_scrape_collector_abandoned to be emitted for the wedged collector")
	}
	if hasMetric(got, "wedged_metric") {
		t.Error("abandoned collector's own metric should not have reached the scrape")
	}
}

// TestWedgedCollectorIsNotRestarted pins the single-flight guard: while a
// collector is still wedged from an earlier scrape it must be skipped, not run
// again. Restarting it would pile a fresh subprocess onto an already saturated
// host once per scrape per Prometheus replica.
func TestWedgedCollectorIsNotRestarted(t *testing.T) {
	old := *maxCollectorDuration
	*maxCollectorDuration = 100 * time.Millisecond
	defer func() { *maxCollectorDuration = old }()

	inFlight.Delete("wedged_once")

	wedged := newWedgedCollector()
	defer wedged.Release()

	n := &NodeCollector{
		logger:     testLogger(),
		Collectors: map[string]Collector{"wedged_once": wedged},
	}

	collect(t, n) // first scrape: starts it, abandons at the budget
	got := collect(t, n)

	if len(wedged.started) != 1 {
		t.Errorf("collector was started %d times; expected exactly 1 while still wedged", len(wedged.started))
	}
	if !hasMetric(got, "node_scrape_collector_stalled") {
		t.Error("expected node_scrape_collector_stalled on the second scrape")
	}
}

// TestAbandonedCollectorNeverWritesToClosedChannel guards the panic risk in the
// design: an abandoned collector must publish into its own staged channel, never
// into the registry's, which is closed once Collect returns.
func TestAbandonedCollectorNeverWritesToClosedChannel(t *testing.T) {
	old := *maxCollectorDuration
	*maxCollectorDuration = 50 * time.Millisecond
	defer func() { *maxCollectorDuration = old }()

	inFlight.Delete("late_writer")

	wedged := newWedgedCollector()
	n := &NodeCollector{
		logger:     testLogger(),
		Collectors: map[string]Collector{"late_writer": wedged},
	}

	ch := make(chan prometheus.Metric, 128)
	n.Collect(ch)
	close(ch) // the registry closes its channel as soon as Collect returns

	// Release afterwards: the collector now writes its metric. If it were still
	// pointed at ch this would panic on a send to a closed channel.
	wedged.Release()
	time.Sleep(100 * time.Millisecond)

	// Reaching here without panicking is the assertion. Confirm the collector
	// was also released for reuse once it finally finished.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !inFlightFor("late_writer").Load() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("collector never released its in-flight marker after finishing")
}

// TestZeroBudgetRestoresBlockingBehaviour confirms the escape hatch: setting the
// flag to 0 reverts to waiting for every collector, matching upstream.
func TestZeroBudgetRestoresBlockingBehaviour(t *testing.T) {
	old := *maxCollectorDuration
	*maxCollectorDuration = 0
	defer func() { *maxCollectorDuration = old }()

	n := &NodeCollector{
		logger:     testLogger(),
		Collectors: map[string]Collector{"fast_a": fastCollector{name: "fast_a"}},
	}

	got := collect(t, n)
	if !hasMetric(got, "fast_a_metric") {
		t.Error("expected the collector to run normally with the budget disabled")
	}
	if hasMetric(got, "node_scrape_collector_abandoned") {
		t.Error("no collector should be abandoned when the budget is disabled")
	}
}
