// collector/diskqueues_aix.go
//go:build aix
// +build aix

package collector

/*
#cgo LDFLAGS: -lperfstat
#include <libperfstat.h>
#include <stdlib.h>
#include <string.h>
#include <errno.h>

// Count of block devices (disks).
static int perfstat_disk_count() {
    return perfstat_disk(NULL, NULL, sizeof(perfstat_disk_t), 0);
}

// Fill perfstat_disk_t records for n devices starting from FIRST_DISK (or "hdisk0").
// Returns number of records filled, or <0 on error.
static int perfstat_disk_fill_first(perfstat_disk_t *buf, int n) {
    perfstat_id_t first;
#ifdef FIRST_DISK
    strcpy(first.name, FIRST_DISK);
#else
    // Fallback start-id; adjust if your device naming differs.
    strcpy(first.name, "");
#endif
    return perfstat_disk(&first, buf, sizeof(perfstat_disk_t), n);
}

// Expose errno for troubleshooting.
static int last_errno() { return errno; }
*/
import "C"

import (
	"fmt"
	"log/slog"
	"unsafe"

	"github.com/prometheus/client_golang/prometheus"
)

// Register the collector; disabled by default. Enable with --collector.aix_diskqueues
func init() {
	registerCollector("aix_diskqueues", defaultDisabled, NewAIXDiskQueuesCollector)
}

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

// Constructor: latest node_exporter master signature.
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
			"Minimum observed wait-queue time.",
			[]string{"device"}, nil,
		),
		dqWaitSecondsMaxDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "disk", "wait_queue_seconds_max"),
			"Maximum observed wait-queue time.",
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
			"Minimum observed read service time.",
			[]string{"device"}, nil,
		),
		rServSecondsMaxDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "disk", "read_service_seconds_max"),
			"Maximum observed read service time.",
			[]string{"device"}, nil,
		),
		wServSecondsDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "disk", "write_service_seconds_total"),
			"Cumulative write service time.",
			[]string{"device"}, nil,
		),
		wServSecondsMinDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "disk", "write_service_seconds_min"),
			"Minimum observed write service time.",
			[]string{"device"}, nil,
		),
		wServSecondsMaxDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "disk", "write_service_seconds_max"),
			"Maximum observed write service time.",
			[]string{"device"}, nil,
		),
		logger: logger,
	}
	return c, nil
}

// Update reads libperfstat and emits node_* metrics for each disk.
// NOTE: perfstat_disk_t fields expected (AIX): name, wq_depth, wq_time, wq_min_time, wq_max_time,
//
//	wq_sampled, q_full, qdepth, rserv, wserv, min_rserv, max_rserv, min_wserv, max_wserv.
//
// If your TL/ML uses slightly different names, tweak the field accesses accordingly.
func (c *aixDiskQueuesCollector) Update(ch chan<- prometheus.Metric) error {
	// 1) Device count
	n := int(C.perfstat_disk_count())
	if n <= 0 {
		return fmt.Errorf("perfstat_disk_count returned %d", n)
	}

	// 2) Allocate array
	size := C.size_t(n) * C.size_t(C.sizeof_perfstat_disk_t)
	buf := C.malloc(size)
	if buf == nil {
		return fmt.Errorf("malloc failed for perfstat_disk_t buffer")
	}
	defer C.free(buf)

	// 3) Fill records starting from FIRST_DISK/"hdisk0"
	filled := int(C.perfstat_disk_fill_first((*C.perfstat_disk_t)(buf), C.int(n)))
	if filled <= 0 {
		errNo := int(C.last_errno())
		c.logger.Error("perfstat_disk_fill_first failed", "filled", filled, "errno", errNo, "n", n)
		return fmt.Errorf("perfstat_disk_fill_first returned %d (errno=%d)", filled, errNo)
	}
	if filled != n {
		c.logger.Warn("perfstat_disk_fill_first count mismatch", "filled", filled, "expected", n)
	}

	// 4) Emit per-device metrics
	for i := 0; i < filled; i++ {
		rec := (*C.perfstat_disk_t)(unsafe.Pointer(uintptr(unsafe.Pointer(buf)) + uintptr(i)*uintptr(C.sizeof_perfstat_disk_t)))

		// Device label from C string field 'name'
		device := C.GoString(&rec.name[0])
		const us = 1e6

		ch <- prometheus.MustNewConstMetric(c.dqWaitSecondsDesc, prometheus.CounterValue, float64(rec.wq_time)/us, device)
		ch <- prometheus.MustNewConstMetric(c.dqWaitSecondsMinDesc, prometheus.GaugeValue, float64(rec.wq_min_time)/us, device)
		ch <- prometheus.MustNewConstMetric(c.dqWaitSecondsMaxDesc, prometheus.GaugeValue, float64(rec.wq_max_time)/us, device)

		ch <- prometheus.MustNewConstMetric(c.rServSecondsDesc, prometheus.CounterValue, float64(rec.rserv)/us, device)
		ch <- prometheus.MustNewConstMetric(c.rServSecondsMinDesc, prometheus.GaugeValue, float64(rec.min_rserv)/us, device)
		ch <- prometheus.MustNewConstMetric(c.rServSecondsMaxDesc, prometheus.GaugeValue, float64(rec.max_rserv)/us, device)

		ch <- prometheus.MustNewConstMetric(c.wServSecondsDesc, prometheus.CounterValue, float64(rec.wserv)/us, device)
		ch <- prometheus.MustNewConstMetric(c.wServSecondsMinDesc, prometheus.GaugeValue, float64(rec.min_wserv)/us, device)
		ch <- prometheus.MustNewConstMetric(c.wServSecondsMaxDesc, prometheus.GaugeValue, float64(rec.max_wserv)/us, device)
		ch <- prometheus.MustNewConstMetric(c.dqWaitDepthDesc, prometheus.GaugeValue, float64(rec.wq_depth), device)
		ch <- prometheus.MustNewConstMetric(c.dqWaitSampledDesc, prometheus.CounterValue, float64(rec.wq_sampled), device)
		ch <- prometheus.MustNewConstMetric(c.dqQueueFullDesc, prometheus.CounterValue, float64(rec.q_full), device)
		ch <- prometheus.MustNewConstMetric(c.dqServiceDepthDesc, prometheus.GaugeValue, float64(rec.qdepth), device)
	}

	return nil
}
