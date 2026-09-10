// collector/timebase_aix.go
//go:build aix
// +build aix

package collector

/*
#cgo LDFLAGS: -lperfstat
#include <libperfstat.h>
#include <stdlib.h>
#include <string.h>
#include <errno.h>

// Number of logical CPUs.
static int perfstat_cpu_count() {
    return perfstat_cpu(NULL, NULL, sizeof(perfstat_cpu_t), 0);
}

// Fill perfstat_cpu_t records for n logical CPUs starting from FIRST_CPU (or "cpu0").
// Returns count of records filled, or <0 on error.
static int perfstat_cpu_fill_first(perfstat_cpu_t *buf, int n) {
    perfstat_id_t first;
#ifdef FIRST_CPU
    strcpy(first.name, FIRST_CPU);
#else
    strcpy(first.name, "cpu0");
#endif
    return perfstat_cpu(&first, buf, sizeof(perfstat_cpu_t), n);
}

// Expose errno to Go for troubleshooting.
static int last_errno() { return errno; }
*/
import "C"

import (
	"fmt"
	"log/slog"
	"unsafe"

	"github.com/prometheus/client_golang/prometheus"
)

// Register the collector; disabled by default. Enable with --collector.aix_timebase
func init() {
	// node_exporter master expects a ctor func(*slog.Logger) (Collector, error)
	registerCollector("aix_timebase", defaultDisabled, NewAIXTimebaseCollector)
}

type aixTimebaseCollector struct {
	// Primary node_* metrics (recommended)
	nodeTbLastDesc   *prometheus.Desc // PURR timebase ticks (snapshot)
	nodeVtbLastDesc  *prometheus.Desc // SPURR virtual timebase ticks (snapshot)
	nodeIcntLastDesc *prometheus.Desc // Instruction count (snapshot)

	logger *slog.Logger
}

// NewAIXTimebaseCollector matches the required signature on master.
func NewAIXTimebaseCollector(logger *slog.Logger) (Collector, error) {
	if logger == nil {
		logger = slog.Default()
	}
	c := &aixTimebaseCollector{
		// Primary node_* series
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
// (If Update returns error, Prometheus reports node_scrape_collector_success=0.)
func (c *aixTimebaseCollector) Update(ch chan<- prometheus.Metric) error {
	// 1) Logical CPUs on this LPAR
	n := int(C.perfstat_cpu_count())
	if n <= 0 {
		return fmt.Errorf("perfstat_cpu_count returned %d", n)
	}

	// 2) Allocate perfstat_cpu_t array
	size := C.size_t(n) * C.size_t(C.sizeof_perfstat_cpu_t)
	buf := C.malloc(size)
	if buf == nil {
		return fmt.Errorf("malloc failed for perfstat_cpu_t buffer")
	}
	defer C.free(buf)

	// 3) Fill records using FIRST_CPU/"cpu0" start id (robust across AIX TL/ML)
	filled := int(C.perfstat_cpu_fill_first((*C.perfstat_cpu_t)(buf), C.int(n)))
	if filled <= 0 {
		errNo := int(C.last_errno())
		c.logger.Error("perfstat_cpu_fill_first failed", "filled", filled, "errno", errNo, "n", n)
		return fmt.Errorf("perfstat_cpu_fill_first returned %d (errno=%d)", filled, errNo)
	}
	if filled != n {
		c.logger.Warn("perfstat_cpu_fill_first count mismatch", "filled", filled, "expected", n)
	}

	// 4) Emit metrics per logical CPU (gauges: snapshots)
	for i := 0; i < filled; i++ {
		rec := (*C.perfstat_cpu_t)(unsafe.Pointer(uintptr(unsafe.Pointer(buf)) + uintptr(i)*uintptr(C.sizeof_perfstat_cpu_t)))
		cpuLabel := fmt.Sprintf("cpu%d", i)

		// node_* (recommended)
		ch <- prometheus.MustNewConstMetric(c.nodeTbLastDesc, prometheus.GaugeValue, float64(rec.tb_last), cpuLabel)
		ch <- prometheus.MustNewConstMetric(c.nodeVtbLastDesc, prometheus.GaugeValue, float64(rec.vtb_last), cpuLabel)
		ch <- prometheus.MustNewConstMetric(c.nodeIcntLastDesc, prometheus.GaugeValue, float64(rec.icount_last), cpuLabel)
	}
	return nil
}
