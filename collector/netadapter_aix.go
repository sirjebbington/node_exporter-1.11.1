// collector/netadapter_aix.go
//go:build aix
// +build aix

package collector

/*
#cgo LDFLAGS: -lperfstat
#include <libperfstat.h>
#include <stdlib.h>
#include <string.h>
#include <errno.h>

// Count net adapters
static int perfstat_netadapter_count() {
    return perfstat_netadapter(NULL, NULL, sizeof(perfstat_netadapter_t), 0);
}

// Fill perfstat_netadapter_t starting from FIRST_NETADAPTER
static int perfstat_netadapter_fill_first(perfstat_netadapter_t *buf, int n) {
    perfstat_id_t first;
#ifdef FIRST_NETADAPTER
    strcpy(first.name, FIRST_NETADAPTER);
#else
    // "" also means start-from-first according to perfstat patterns
    strcpy(first.name, "");
#endif
    return perfstat_netadapter(&first, buf, sizeof(perfstat_netadapter_t), n);
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

func (c *aixNetAdapterCollector) Update(ch chan<- prometheus.Metric) error {
	// 1) Count adapters
	n := int(C.perfstat_netadapter_count())
	if n <= 0 {
		return fmt.Errorf("perfstat_netadapter_count returned %d", n)
	}

	// 2) Allocate array
	size := C.size_t(n) * C.size_t(C.sizeof_perfstat_netadapter_t)
	buf := C.malloc(size)
	if buf == nil {
		return fmt.Errorf("malloc failed for perfstat_netadapter_t buffer")
	}
	defer C.free(buf)

	// 3) Fill records
	filled := int(C.perfstat_netadapter_fill_first((*C.perfstat_netadapter_t)(buf), C.int(n)))
	if filled <= 0 {
		errNo := int(C.last_errno())
		c.logger.Error("perfstat_netadapter_fill_first failed", "filled", filled, "errno", errNo, "n", n)
		return fmt.Errorf("perfstat_netadapter_fill_first returned %d (errno=%d)", filled, errNo)
	}
	if filled != n {
		c.logger.Warn("perfstat_netadapter_fill_first count mismatch", "filled", filled, "expected", n)
	}

	// 4) Emit per adapter
	for i := 0; i < filled; i++ {
		rec := (*C.perfstat_netadapter_t)(unsafe.Pointer(uintptr(unsafe.Pointer(buf)) + uintptr(i)*uintptr(C.sizeof_perfstat_netadapter_t)))
		device := C.GoString(&rec.name[0])

		// ---- Interrupts ----
		// Likely field names: rx_interrupts / tx_interrupts (adjust if needed):
		//   grep -n 'rx_.*interrupt' /usr/include/libperfstat.h
		//   grep -n 'tx_.*interrupt' /usr/include/libperfstat.h
		ch <- prometheus.MustNewConstMetric(c.intrDesc, prometheus.CounterValue, float64(rec.rx_interrupts), device, "rx")
		ch <- prometheus.MustNewConstMetric(c.intrDesc, prometheus.CounterValue, float64(rec.tx_interrupts), device, "tx")

		// ---- Errors (generic) ----
		// Likely field names: rx_errors / tx_errors:
		//   grep -n 'rx_errors' /usr/include/libperfstat.h
		//   grep -n 'tx_errors' /usr/include/libperfstat.h
		ch <- prometheus.MustNewConstMetric(c.errsDesc, prometheus.CounterValue, float64(rec.rx_errors), device, "rx")
		ch <- prometheus.MustNewConstMetric(c.errsDesc, prometheus.CounterValue, float64(rec.tx_errors), device, "tx")

		// ---- Drops ----
		// Likely field names: rx_packets_dropped / tx_packets_dropped:
		//   grep -n 'packets_dropped' /usr/include/libperfstat.h
		ch <- prometheus.MustNewConstMetric(c.dropDesc, prometheus.CounterValue, float64(rec.rx_packets_dropped), device, "rx")
		ch <- prometheus.MustNewConstMetric(c.dropDesc, prometheus.CounterValue, float64(rec.tx_packets_dropped), device, "tx")

		// ---- Extensions ready to add (uncomment once verified on your header) ----
		// CRC/alignment/collision/timeout/lost CTS, etc., map 1:1 to distinct series:
		//   node_netadapter_errors_total{device,dir="rx",type="crc|alignment|noresource|toolong|tooshort"}
		//   node_netadapter_errors_total{device,dir="tx",type="timeout|max_collision|late_collision|lost_cts"}
		// Example fields often present:
		// ch <- prometheus.MustNewConstMetric(c.errsTypedDesc, prometheus.CounterValue, float64(rec.rx_CRC_errors), device, "rx", "crc")
		// ... (add a typed-desc family if you wish to expose per-type series)
	}

	return nil
}
