// collector/scheduler_aix.go
//go:build aix
// +build aix

package collector

import (
	"fmt"
	"log/slog"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

// Register; disabled by default. Enable with --collector.aix_scheduler
func init() {
	registerCollector("aix_scheduler", defaultDisabled, NewAIXSchedulerCollector)
}

type aixSchedulerCollector struct {
	// Context switches
	ctxTotalDesc *prometheus.Desc // node_sched_context_switches_total{cpu}
	ctxVolDesc   *prometheus.Desc // node_sched_context_switches_voluntary_total{cpu}
	ctxInvolDesc *prometheus.Desc // node_sched_context_switches_involuntary_total{cpu}

	// Dispatch granularity
	localDispDesc *prometheus.Desc // node_sched_local_dispatch_total{cpu}
	nearDispDesc  *prometheus.Desc // node_sched_near_dispatch_total{cpu}
	farDispDesc   *prometheus.Desc // node_sched_far_dispatch_total{cpu}

	// Redispatch per SD domain
	redispDesc *prometheus.Desc // node_sched_redispatch_total{cpu,domain}

	logger *slog.Logger
}

func NewAIXSchedulerCollector(logger *slog.Logger) (Collector, error) {
	if logger == nil {
		logger = slog.Default()
	}
	ns := namespace // "node"
	return &aixSchedulerCollector{
		ctxTotalDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "sched", "context_switches_total"),
			"Total context switches per logical CPU.",
			[]string{"cpu"}, nil,
		),
		ctxVolDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "sched", "context_switches_voluntary_total"),
			"Voluntary context switches per logical CPU.",
			[]string{"cpu"}, nil,
		),
		ctxInvolDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "sched", "context_switches_involuntary_total"),
			"Involuntary context switches per logical CPU.",
			[]string{"cpu"}, nil,
		),
		localDispDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "sched", "local_dispatch_total"),
			"Local dispatch events per logical CPU.",
			[]string{"cpu"}, nil,
		),
		nearDispDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "sched", "near_dispatch_total"),
			"Near dispatch events per logical CPU.",
			[]string{"cpu"}, nil,
		),
		farDispDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "sched", "far_dispatch_total"),
			"Far dispatch events per logical CPU.",
			[]string{"cpu"}, nil,
		),
		redispDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "sched", "redispatch_total"),
			"Redispatch events per scheduling domain.",
			[]string{"cpu", "domain"}, nil,
		),
		logger: logger,
	}, nil
}

func (c *aixSchedulerCollector) Update(ch chan<- prometheus.Metric) error {
	cpus, err := cpuStat()
	if err != nil {
		return fmt.Errorf("perfstat CpuStat: %w", err)
	}

	for n, stat := range cpus {
		// Index rather than stat.Name, to join with the default cpu
		// collector — see the same note in timebase_aix.go.
		cpu := strconv.Itoa(n)

		// ---- Context switches ----
		ch <- prometheus.MustNewConstMetric(c.ctxTotalDesc, prometheus.CounterValue, float64(stat.CSwitches), cpu)
		ch <- prometheus.MustNewConstMetric(c.ctxVolDesc, prometheus.CounterValue, float64(stat.VolCSwitch), cpu)
		ch <- prometheus.MustNewConstMetric(c.ctxInvolDesc, prometheus.CounterValue, float64(stat.InvolCSwitch), cpu)

		// ---- Dispatch granularity ----
		ch <- prometheus.MustNewConstMetric(c.localDispDesc, prometheus.CounterValue, float64(stat.LocalDispatch), cpu)
		ch <- prometheus.MustNewConstMetric(c.nearDispDesc, prometheus.CounterValue, float64(stat.NearDispatch), cpu)
		ch <- prometheus.MustNewConstMetric(c.farDispDesc, prometheus.CounterValue, float64(stat.FarDispatch), cpu)

		// ---- Redispatch across scheduler affinity domains ----
		ch <- prometheus.MustNewConstMetric(c.redispDesc, prometheus.CounterValue, float64(stat.RedispSD0), cpu, "sd0")
		ch <- prometheus.MustNewConstMetric(c.redispDesc, prometheus.CounterValue, float64(stat.RedispSD1), cpu, "sd1")
		ch <- prometheus.MustNewConstMetric(c.redispDesc, prometheus.CounterValue, float64(stat.RedispSD2), cpu, "sd2")
		ch <- prometheus.MustNewConstMetric(c.redispDesc, prometheus.CounterValue, float64(stat.RedispSD3), cpu, "sd3")
		ch <- prometheus.MustNewConstMetric(c.redispDesc, prometheus.CounterValue, float64(stat.RedispSD4), cpu, "sd4")
		ch <- prometheus.MustNewConstMetric(c.redispDesc, prometheus.CounterValue, float64(stat.RedispSD5), cpu, "sd5")
	}
	return nil
}
