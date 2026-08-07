// collector/diskqueues_aix.go
//go:build aix
// +build aix

package collector

import (
	"fmt"
	"log/slog"

	"github.com/power-devops/perfstat"
	"github.com/prometheus/client_golang/prometheus"
)

// Register the collector; disabled by default. Enable with --collector.aix_diskqueues
func init() {
	registerCollector("aix_diskqueues", defaultDisabled, NewAIXDiskQueuesCollector)
}

// diskTimeNanoseconds converts libperfstat's disk timing fields to seconds.
//
// The rserv/wserv/wq_time family in perfstat_disk_t is reported in
// nanoseconds. This matches the upstream node_exporter AIX diskstats collector
// (diskstats_aix.go), which divides the same fields by 1e9 for
// node_disk_read_time_seconds_total.
//
// This collector previously divided by 1e6, so its service-time series read
// 1000x higher than diskstats' values derived from the identical field. To
// confirm the scaling on a host: `iostat -D hdisk0` prints avg serv in
// milliseconds; node_disk_read_service_seconds_total * 1000 should land in the
// same range.
const diskTimeNanoseconds = 1e9

// Collector struct with descriptors for all node_* metrics we emit.
type aixDiskQueuesCollector struct {
	// Wait-queue (I/O queuing pressure)
	dqWaitDepthDesc      *prometheus.Desc // node_disk_wait_queue_depth{device} gauge
	dqWaitSecondsDesc    *prometheus.Desc // node_disk_wait_queue_seconds_total{device} counter
	dqWaitSecondsMinDesc *prometheus.Desc // node_disk_wait_queue_seconds_min{device} gauge
	dqWaitSecondsMaxDesc *prometheus.Desc // node_disk_wait_queue_seconds_max{device} gauge
	dqWaitSampledDesc    *prometheus.Desc // node_disk_wait_queue_samples_total{device} counter
	dqQueueFullDesc      *prometheus.Desc // node_disk_queue_full_total{device} counter
	dqServiceDepthDesc   *prometheus.Desc // node_disk_service_queue_depth{device} gauge

	// Service times (read/write, min/max)
	rServSecondsDesc    *prometheus.Desc // node_disk_read_service_seconds_total{device} counter
	rServSecondsMinDesc *prometheus.Desc // node_disk_read_service_seconds_min{device} gauge
	rServSecondsMaxDesc *prometheus.Desc // node_disk_read_service_seconds_max{device} gauge
	wServSecondsDesc    *prometheus.Desc // node_disk_write_service_seconds_total{device} counter
	wServSecondsMinDesc *prometheus.Desc // node_disk_write_service_seconds_min{device} gauge
	wServSecondsMaxDesc *prometheus.Desc // node_disk_write_service_seconds_max{device} gauge

	logger *slog.Logger
}

func NewAIXDiskQueuesCollector(logger *slog.Logger) (Collector, error) {
	if logger == nil {
		logger = slog.Default()
	}
	ns := namespace // "node"
	c := &aixDiskQueuesCollector{
		// Wait-queue metrics
		dqWaitDepthDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "disk", "wait_queue_depth"),
			"Current depth of the disk wait-queue (I/O requests waiting).",
			[]string{"device"}, nil,
		),
		dqWaitSecondsDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "disk", "wait_queue_seconds_total"),
			"Cumulative time spent with requests waiting in the queue.",
			[]string{"device"}, nil,
		),
		dqWaitSecondsMinDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "disk", "wait_queue_seconds_min"),
			"Minimum observed wait-queue time since boot.",
			[]string{"device"}, nil,
		),
		dqWaitSecondsMaxDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "disk", "wait_queue_seconds_max"),
			"Maximum observed wait-queue time since boot.",
			[]string{"device"}, nil,
		),
		dqWaitSampledDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "disk", "wait_queue_samples_total"),
			"Number of samples taken for wait-queue timing.",
			[]string{"device"}, nil,
		),
		dqQueueFullDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "disk", "queue_full_total"),
			"Number of times the service queue was full.",
			[]string{"device"}, nil,
		),
		dqServiceDepthDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "disk", "service_queue_depth"),
			"Current depth of the service queue (requests being serviced).",
			[]string{"device"}, nil,
		),

		// Service times
		rServSecondsDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "disk", "read_service_seconds_total"),
			"Cumulative read service time.",
			[]string{"device"}, nil,
		),
		rServSecondsMinDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "disk", "read_service_seconds_min"),
			"Minimum observed read service time since boot.",
			[]string{"device"}, nil,
		),
		rServSecondsMaxDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "disk", "read_service_seconds_max"),
			"Maximum observed read service time since boot.",
			[]string{"device"}, nil,
		),
		wServSecondsDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "disk", "write_service_seconds_total"),
			"Cumulative write service time.",
			[]string{"device"}, nil,
		),
		wServSecondsMinDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "disk", "write_service_seconds_min"),
			"Minimum observed write service time since boot.",
			[]string{"device"}, nil,
		),
		wServSecondsMaxDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "disk", "write_service_seconds_max"),
			"Maximum observed write service time since boot.",
			[]string{"device"}, nil,
		),
		logger: logger,
	}
	return c, nil
}

// Update reads libperfstat via perfstat.DiskStat() and emits node_* metrics
// for each disk.
//
// This replaces a hand-written cgo preamble that called perfstat_disk()
// directly with manual malloc and unsafe pointer arithmetic. The library does
// the same call, with the buffer sizing and record walk already correct, and
// is the same dependency the default cpu/diskstats/netdev collectors use.
func (c *aixDiskQueuesCollector) Update(ch chan<- prometheus.Metric) error {
	disks, err := perfstat.DiskStat()
	if err != nil {
		return fmt.Errorf("perfstat DiskStat: %w", err)
	}

	for _, d := range disks {
		device := d.Name

		ch <- prometheus.MustNewConstMetric(c.dqWaitSecondsDesc, prometheus.CounterValue, float64(d.WqTime)/diskTimeNanoseconds, device)
		ch <- prometheus.MustNewConstMetric(c.dqWaitSecondsMinDesc, prometheus.GaugeValue, float64(d.WqMinTime)/diskTimeNanoseconds, device)
		ch <- prometheus.MustNewConstMetric(c.dqWaitSecondsMaxDesc, prometheus.GaugeValue, float64(d.WqMaxTime)/diskTimeNanoseconds, device)

		ch <- prometheus.MustNewConstMetric(c.rServSecondsDesc, prometheus.CounterValue, float64(d.Rserv)/diskTimeNanoseconds, device)
		ch <- prometheus.MustNewConstMetric(c.rServSecondsMinDesc, prometheus.GaugeValue, float64(d.MinRserv)/diskTimeNanoseconds, device)
		ch <- prometheus.MustNewConstMetric(c.rServSecondsMaxDesc, prometheus.GaugeValue, float64(d.MaxRserv)/diskTimeNanoseconds, device)

		ch <- prometheus.MustNewConstMetric(c.wServSecondsDesc, prometheus.CounterValue, float64(d.Wserv)/diskTimeNanoseconds, device)
		ch <- prometheus.MustNewConstMetric(c.wServSecondsMinDesc, prometheus.GaugeValue, float64(d.MinWserv)/diskTimeNanoseconds, device)
		ch <- prometheus.MustNewConstMetric(c.wServSecondsMaxDesc, prometheus.GaugeValue, float64(d.MaxWserv)/diskTimeNanoseconds, device)

		ch <- prometheus.MustNewConstMetric(c.dqWaitDepthDesc, prometheus.GaugeValue, float64(d.WqDepth), device)
		ch <- prometheus.MustNewConstMetric(c.dqWaitSampledDesc, prometheus.CounterValue, float64(d.WqSampled), device)
		ch <- prometheus.MustNewConstMetric(c.dqQueueFullDesc, prometheus.CounterValue, float64(d.QFull), device)
		ch <- prometheus.MustNewConstMetric(c.dqServiceDepthDesc, prometheus.GaugeValue, float64(d.QDepth), device)
	}

	return nil
}
