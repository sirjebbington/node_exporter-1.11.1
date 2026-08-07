// collector/timebase_aix.go
//go:build aix
// +build aix

package collector

import (
	"fmt"
	"log/slog"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

// Register the collector; disabled by default. Enable with --collector.aix_timebase
func init() {
	registerCollector("aix_timebase", defaultDisabled, NewAIXTimebaseCollector)
}

type aixTimebaseCollector struct {
	nodeTbLastDesc   *prometheus.Desc // PURR timebase ticks (snapshot)
	nodeVtbLastDesc  *prometheus.Desc // SPURR virtual timebase ticks (snapshot)
	nodeIcntLastDesc *prometheus.Desc // Instruction count (snapshot)

	logger *slog.Logger
}

func NewAIXTimebaseCollector(logger *slog.Logger) (Collector, error) {
	if logger == nil {
		logger = slog.Default()
	}
	c := &aixTimebaseCollector{
		nodeTbLastDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "cpu", "timebase_ticks_last"),
			"Last timebase (PURR) ticks snapshot per logical CPU.",
			[]string{"cpu"}, nil,
		),
		nodeVtbLastDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "cpu", "virtual_timebase_ticks_last"),
			"Last virtual timebase (SPURR) ticks snapshot per logical CPU.",
			[]string{"cpu"}, nil,
		),
		nodeIcntLastDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "cpu", "instructions_last"),
			"Last instruction count snapshot per logical CPU.",
			[]string{"cpu"}, nil,
		),
		logger: logger,
	}
	return c, nil
}

// Update reads libperfstat and emits node_* gauges.
// (If Update returns an error, node_scrape_collector_success goes to 0.)
func (c *aixTimebaseCollector) Update(ch chan<- prometheus.Metric) error {
	cpus, err := cpuStat()
	if err != nil {
		return fmt.Errorf("perfstat CpuStat: %w", err)
	}

	for n, stat := range cpus {
		// The slice index, not stat.Name, so this joins with the default cpu
		// collector's node_cpu_seconds_total{cpu="0"} — cpu_aix.go labels by
		// index over the same perfstat.CpuStat() slice.
		cpu := strconv.Itoa(n)

		ch <- prometheus.MustNewConstMetric(c.nodeTbLastDesc, prometheus.GaugeValue, float64(stat.TbLast), cpu)
		ch <- prometheus.MustNewConstMetric(c.nodeVtbLastDesc, prometheus.GaugeValue, float64(stat.VtbLast), cpu)
		ch <- prometheus.MustNewConstMetric(c.nodeIcntLastDesc, prometheus.GaugeValue, float64(stat.ICountLast), cpu)
	}
	return nil
}
