// collector/netadapter_aix.go
//go:build aix
// +build aix

package collector

import (
	"fmt"
	"log/slog"

	"github.com/power-devops/perfstat"
	"github.com/prometheus/client_golang/prometheus"
)

// Register; disabled by default. Enable with --collector.aix_netadapter
func init() {
	registerCollector("aix_netadapter", defaultDisabled, NewAIXNetAdapterCollector)
}

type aixNetAdapterCollector struct {
	// Interrupts per dir
	intrDesc *prometheus.Desc // node_netadapter_interrupts_total{device,dir="rx|tx"}
	// Generic errors per dir
	errsDesc *prometheus.Desc // node_netadapter_errors_total{device,dir="rx|tx"}
	// Packets dropped per dir
	dropDesc *prometheus.Desc // node_netadapter_packets_dropped_total{device,dir="rx|tx"}

	logger *slog.Logger
}

func NewAIXNetAdapterCollector(logger *slog.Logger) (Collector, error) {
	if logger == nil {
		logger = slog.Default()
	}
	ns := namespace
	return &aixNetAdapterCollector{
		intrDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "netadapter", "interrupts_total"),
			"Network adapter interrupts by direction.",
			[]string{"device", "dir"}, nil,
		),
		errsDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "netadapter", "errors_total"),
			"Network adapter errors by direction.",
			[]string{"device", "dir"}, nil,
		),
		dropDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "netadapter", "packets_dropped_total"),
			"Network adapter packets dropped by direction.",
			[]string{"device", "dir"}, nil,
		),
		logger: logger,
	}, nil
}

// Update emits per-adapter counters from perfstat_netadapter().
//
// Note that the default-enabled netdev collector (netdev_aix.go) reads the
// same libperfstat table and already exports RxErrors/TxErrors and
// RxPacketsDropped/TxPacketsDropped as node_network_{receive,transmit}_errors_total
// and node_network_{receive,transmit}_drop_total. Of what this collector emits,
// only the interrupt counters have no netdev equivalent.
func (c *aixNetAdapterCollector) Update(ch chan<- prometheus.Metric) error {
	adapters, err := perfstat.NetAdapterStat()
	if err != nil {
		return fmt.Errorf("perfstat NetAdapterStat: %w", err)
	}

	for _, a := range adapters {
		device := a.Name

		// ---- Interrupts ----
		ch <- prometheus.MustNewConstMetric(c.intrDesc, prometheus.CounterValue, float64(a.RxInterrupts), device, "rx")
		ch <- prometheus.MustNewConstMetric(c.intrDesc, prometheus.CounterValue, float64(a.TxInterrupts), device, "tx")

		// ---- Errors (generic) ----
		ch <- prometheus.MustNewConstMetric(c.errsDesc, prometheus.CounterValue, float64(a.RxErrors), device, "rx")
		ch <- prometheus.MustNewConstMetric(c.errsDesc, prometheus.CounterValue, float64(a.TxErrors), device, "tx")

		// ---- Drops ----
		ch <- prometheus.MustNewConstMetric(c.dropDesc, prometheus.CounterValue, float64(a.RxPacketsDropped), device, "rx")
		ch <- prometheus.MustNewConstMetric(c.dropDesc, prometheus.CounterValue, float64(a.TxPacketsDropped), device, "tx")
	}

	return nil
}
