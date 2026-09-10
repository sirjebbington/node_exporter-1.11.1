// collector/hypervisor_aix.go
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

// Fill perfstat_cpu_t for n logical CPUs starting from FIRST_CPU (or "cpu0").
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

// Register; disabled by default. Enable with --collector.aix_hypervisor
func init() {
	registerCollector("aix_hypervisor", defaultDisabled, NewAIXHypervisorCollector)
}

type aixHypervisorCollector struct {
	// SPURR support flag
	spurrFlagDesc *prometheus.Desc // node_cpu_spurr_enabled{cpu} gauge(0/1)

	// SPURR per-mode ticks (cumulative)
	spurrTicksDesc *prometheus.Desc // node_cpu_spurr_ticks_total{cpu,mode}
	logger         *slog.Logger
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
	// 1) Count CPUs
	n := int(C.perfstat_cpu_count())
	if n <= 0 {
		return fmt.Errorf("perfstat_cpu_count returned %d", n)
	}

	// 2) Allocate array
	size := C.size_t(n) * C.size_t(C.sizeof_perfstat_cpu_t)
	buf := C.malloc(size)
	if buf == nil {
		return fmt.Errorf("malloc failed for perfstat_cpu_t buffer")
	}
	defer C.free(buf)

	// 3) Fill starting from first cpu
	filled := int(C.perfstat_cpu_fill_first((*C.perfstat_cpu_t)(buf), C.int(n)))
	if filled <= 0 {
		errNo := int(C.last_errno())
		c.logger.Error("perfstat_cpu_fill_first failed", "filled", filled, "errno", errNo, "n", n)
		return fmt.Errorf("perfstat_cpu_fill_first returned %d (errno=%d)", filled, errNo)
	}
	if filled != n {
		c.logger.Warn("perfstat_cpu_fill_first count mismatch", "filled", filled, "expected", n)
	}

	// 4) Emit per-CPU
	for i := 0; i < filled; i++ {
		rec := (*C.perfstat_cpu_t)(unsafe.Pointer(uintptr(unsafe.Pointer(buf)) + uintptr(i)*uintptr(C.sizeof_perfstat_cpu_t)))
		cpu := fmt.Sprintf("cpu%d", i)

		// ---- SPURR flag (likely 'spurrflag' in perfstat_cpu_t) ----
		// If your header uses a different name, adjust accordingly:
		//   grep -n 'spurrflag' /usr/include/libperfstat.h
		ch <- prometheus.MustNewConstMetric(c.spurrFlagDesc, prometheus.GaugeValue, float64(rec.spurrflag), cpu)

		// ---- SPURR per-mode ticks ----
		// These fields typically exist as *_spurr — verify on your header:
		//   grep -n 'puser_spurr' /usr/include/libperfstat.h
		//   grep -n 'psys_spurr'  /usr/include/libperfstat.h
		//   grep -n 'pidle_spurr' /usr/include/libperfstat.h
		//   grep -n 'pwait_spurr' /usr/include/libperfstat.h
		ch <- prometheus.MustNewConstMetric(c.spurrTicksDesc, prometheus.CounterValue, float64(rec.puser_spurr), cpu, "puser")
		ch <- prometheus.MustNewConstMetric(c.spurrTicksDesc, prometheus.CounterValue, float64(rec.psys_spurr), cpu, "psys")
		ch <- prometheus.MustNewConstMetric(c.spurrTicksDesc, prometheus.CounterValue, float64(rec.pidle_spurr), cpu, "pidle")
		ch <- prometheus.MustNewConstMetric(c.spurrTicksDesc, prometheus.CounterValue, float64(rec.pwait_spurr), cpu, "pwait")
	}

	return nil
}
