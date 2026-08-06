// collector/scheduler_aix.go
//go:build aix
// +build aix

package collector

/*
#cgo LDFLAGS: -lperfstat
#include <libperfstat.h>
#include <stdlib.h>
#include <string.h>
#include <errno.h>

// Count logical CPUs
static int perfstat_cpu_count() {
    return perfstat_cpu(NULL, NULL, sizeof(perfstat_cpu_t), 0);
}

// Fill perfstat_cpu_t for n CPUs starting from FIRST_CPU (or "cpu0")
static int perfstat_cpu_fill_first(perfstat_cpu_t *buf, int n) {
    perfstat_id_t first;
#ifdef FIRST_CPU
    strcpy(first.name, FIRST_CPU);
#else
    strcpy(first.name, "cpu0");
#endif
    return perfstat_cpu(&first, buf, sizeof(perfstat_cpu_t), n);
}

static int last_errno() { return errno; }
*/
import "C"

import (
	"fmt"
	"log/slog"
	"unsafe"

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
	// 1) How many logical CPUs?
	n := int(C.perfstat_cpu_count())
	if n <= 0 {
		return fmt.Errorf("perfstat_cpu_count returned %d", n)
	}

	// 2) Allocate buffer
	size := C.size_t(n) * C.size_t(C.sizeof_perfstat_cpu_t)
	buf := C.malloc(size)
	if buf == nil {
		return fmt.Errorf("malloc failed for perfstat_cpu_t buffer")
	}
	defer C.free(buf)

	// 3) Fill from FIRST_CPU / "cpu0"
	filled := int(C.perfstat_cpu_fill_first((*C.perfstat_cpu_t)(buf), C.int(n)))
	if filled <= 0 {
		errNo := int(C.last_errno())
		c.logger.Error("perfstat_cpu_fill_first failed", "filled", filled, "errno", errNo, "n", n)
		return fmt.Errorf("perfstat_cpu_fill_first returned %d (errno=%d)", filled, errNo)
	}
	if filled != n {
		c.logger.Warn("perfstat_cpu_fill_first count mismatch", "filled", filled, "expected", n)
	}

	// 4) Emit per-CPU metrics
	for i := 0; i < filled; i++ {
		rec := (*C.perfstat_cpu_t)(
			unsafe.Pointer(uintptr(unsafe.Pointer(buf)) + uintptr(i)*uintptr(C.sizeof_perfstat_cpu_t)),
		)
		cpu := fmt.Sprintf("cpu%d", i)

		// ---- Context switches ----
		// Total (cswitches) + voluntary + involuntary
		ch <- prometheus.MustNewConstMetric(c.ctxTotalDesc, prometheus.CounterValue, float64(rec.cswitches), cpu)
		ch <- prometheus.MustNewConstMetric(c.ctxVolDesc, prometheus.CounterValue, float64(rec.vol_cswitch), cpu)
		ch <- prometheus.MustNewConstMetric(c.ctxInvolDesc, prometheus.CounterValue, float64(rec.invol_cswitch), cpu)

		// ---- Dispatch granularity ----
		ch <- prometheus.MustNewConstMetric(c.localDispDesc, prometheus.CounterValue, float64(rec.localdispatch), cpu)
		ch <- prometheus.MustNewConstMetric(c.nearDispDesc, prometheus.CounterValue, float64(rec.neardispatch), cpu)
		ch <- prometheus.MustNewConstMetric(c.farDispDesc, prometheus.CounterValue, float64(rec.fardispatch), cpu)

		// ---- Redispatch across SD domains ----
		// If your header defines fewer/more SD fields, adjust this list accordingly.
		ch <- prometheus.MustNewConstMetric(c.redispDesc, prometheus.CounterValue, float64(rec.redisp_sd0), cpu, "sd0")
		ch <- prometheus.MustNewConstMetric(c.redispDesc, prometheus.CounterValue, float64(rec.redisp_sd1), cpu, "sd1")
		ch <- prometheus.MustNewConstMetric(c.redispDesc, prometheus.CounterValue, float64(rec.redisp_sd2), cpu, "sd2")
		ch <- prometheus.MustNewConstMetric(c.redispDesc, prometheus.CounterValue, float64(rec.redisp_sd3), cpu, "sd3")
		ch <- prometheus.MustNewConstMetric(c.redispDesc, prometheus.CounterValue, float64(rec.redisp_sd4), cpu, "sd4")
		ch <- prometheus.MustNewConstMetric(c.redispDesc, prometheus.CounterValue, float64(rec.redisp_sd5), cpu, "sd5")
	}

	return nil
}
