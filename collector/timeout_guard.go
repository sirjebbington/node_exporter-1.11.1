// collector/timeout_guard.go
package collector

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const defaultCollectorBudget = 9 * time.Second // fits 10s scrape window

// New metric: emitted only when a collector exceeded its budget this scrape.
var (
	nodeCollectorTimeoutInfo = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "scrape", "collector_timeout"),
		"Set to 1 in this scrape if the collector exceeded its time budget.",
		[]string{"collector", "reason"}, // e.g. reason="lsvg_timeout" or "deadline_exceeded"
		nil,
	)
)

type TimeoutGuard struct {
	name     string
	logger   *slog.Logger
	start    time.Time
	deadline time.Time

	timedOut bool
	reason   string
}

func NewTimeoutGuard(name string, logger *slog.Logger, budget time.Duration) *TimeoutGuard {
	if budget <= 0 {
		budget = defaultCollectorBudget
	}
	now := time.Now()
	return &TimeoutGuard{
		name:     name,
		logger:   logger,
		start:    now,
		deadline: now.Add(budget),
	}
}

// Context covering the remaining time for this collector.
func (g *TimeoutGuard) Context() (context.Context, context.CancelFunc) {
	return context.WithDeadline(context.Background(), g.deadline)
}

// Remaining time; returns 0 if deadline passed.
func (g *TimeoutGuard) TimeRemaining() time.Duration {
	rem := time.Until(g.deadline)
	if rem < 0 {
		return 0
	}
	return rem
}

func (g *TimeoutGuard) MustWithinBudget(reason string) bool {
	if time.Now().After(g.deadline) {
		g.FlagTimeout(reason)
		return false
	}
	return true
}

func (g *TimeoutGuard) FlagTimeout(reason string) {
	if g.timedOut {
		return
	}
	if reason == "" {
		reason = "deadline_exceeded"
	}
	g.timedOut = true
	g.reason = reason
	if g.logger != nil {
		g.logger.Warn("collector timed out", "collector", g.name, "reason", g.reason, "budget", g.deadline.Sub(g.start))
	}
}

func (g *TimeoutGuard) EmitIfTimedOut(ch chan<- prometheus.Metric) {
	if !g.timedOut {
		return
	}
	ch <- prometheus.MustNewConstMetric(nodeCollectorTimeoutInfo, prometheus.GaugeValue, 1, g.name, g.reason)
}
