// collector/hypervisor_aix.go
//go:build aix
// +build aix

package collector

import (
	"fmt"
	"log/slog"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

// Register; disabled by default. Enable with --collector.aix_hypervisor
func init() {
	registerCollector("aix_hypervisor", defaultDisabled, NewAIXHypervisorCollector)
}

type aixHypervisorCollector struct {
	// SPURR support flag
	spurrFlagDesc *prometheus.Desc // node_cpu_spurr_enabled{cpu} gauge(0/1)

	// SPURR per-mode ticks (cumulative)
	spurrTicksDesc *prometheus.Desc // node_cpu_spurr_ticks_total{cpu,mode}

	logger *slog.Logger
}

func NewAIXHypervisorCollector(logger *slog.Logger) (Collector, error) {
	if logger == nil {
		logger = slog.Default()
	}
	ns := namespace // "node"
	return &aixHypervisorCollector{
		spurrFlagDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "cpu", "spurr_enabled"),
			"Whether SPURR is enabled (1) or not (0) for this logical CPU.",
			[]string{"cpu"}, nil,
		),
		spurrTicksDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "cpu", "spurr_ticks_total"),
			"Cumulative SPURR ticks per mode (puser, psys, pidle, pwait) for this logical CPU.",
			[]string{"cpu", "mode"}, nil,
		),
		logger: logger,
	}, nil
}

func (c *aixHypervisorCollector) Update(ch chan<- prometheus.Metric) error {
	cpus, err := cpuStat()
	if err != nil {
		return fmt.Errorf("perfstat CpuStat: %w", err)
	}

	for n, stat := range cpus {
		// Index rather than stat.Name, to join with the default cpu
		// collector — see the same note in timebase_aix.go.
		cpu := strconv.Itoa(n)

		ch <- prometheus.MustNewConstMetric(c.spurrFlagDesc, prometheus.GaugeValue, float64(stat.SpurrFlag), cpu)

		ch <- prometheus.MustNewConstMetric(c.spurrTicksDesc, prometheus.CounterValue, float64(stat.PUserSpurr), cpu, "puser")
		ch <- prometheus.MustNewConstMetric(c.spurrTicksDesc, prometheus.CounterValue, float64(stat.PSysSpurr), cpu, "psys")
		ch <- prometheus.MustNewConstMetric(c.spurrTicksDesc, prometheus.CounterValue, float64(stat.PIdleSpurr), cpu, "pidle")
		ch <- prometheus.MustNewConstMetric(c.spurrTicksDesc, prometheus.CounterValue, float64(stat.PWaitSpurr), cpu, "pwait")
	}
	return nil
}
