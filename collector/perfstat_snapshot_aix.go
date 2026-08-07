// collector/perfstat_snapshot_aix.go
//go:build aix
// +build aix

package collector

import (
	"sync"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/power-devops/perfstat"
)

// aixCPUSnapshotTTL bounds how long one perfstat_cpu() sweep is reused.
//
// Four collectors read the same libperfstat per-CPU table: the default `cpu`
// collector plus aix_hypervisor, aix_scheduler and aix_timebase. Each call
// walks every logical CPU on the LPAR, and NodeCollector.Collect runs the
// enabled collectors concurrently, so a 64-way LPAR scraped by two Prometheus
// HA replicas (rca.md §8.3) performed up to eight full sweeps per scrape cycle
// where one would do.
//
// The values are cumulative counters, so serving a slightly older sweep cannot
// corrupt a rate() — it only shifts the sample's effective timestamp by less
// than the TTL, against a scrape interval two orders of magnitude larger. A TTL
// well under the scrape interval also means each scrape still gets a fresh
// read; the cache only ever collapses the duplicates *within* one cycle.
//
// Set to 0 to give every collector its own sweep, restoring the previous
// behaviour exactly.
var aixCPUSnapshotTTL = kingpin.Flag(
	"collector.aix_cpu_snapshot.ttl",
	"How long one libperfstat per-CPU sweep is shared between the aix_hypervisor, aix_scheduler and aix_timebase collectors. 0 disables sharing.",
).Default("2s").Duration()

// cpuSnapshot caches the result of one perfstat.CpuStat() call.
//
// The mutex is held across the CGo call rather than only around the field
// updates. That is deliberate: it gives the cache singleflight semantics, so
// concurrent collectors coalesce onto one sweep instead of racing to issue
// three and having two of them thrown away. libperfstat is documented by IBM
// as threadsafe, so concurrent calls would be *safe* — just wasteful.
type cpuSnapshot struct {
	mu   sync.Mutex
	at   time.Time
	cpus []perfstat.CPU
	err  error
}

var sharedCPUSnapshot cpuSnapshot

// cpuStat returns the per-logical-CPU records from libperfstat, reusing a
// recent sweep when one is available.
//
// The returned slice is shared between callers and must be treated as
// read-only.
func cpuStat() ([]perfstat.CPU, error) {
	ttl := *aixCPUSnapshotTTL
	if ttl <= 0 {
		return perfstat.CpuStat()
	}

	s := &sharedCPUSnapshot
	s.mu.Lock()
	defer s.mu.Unlock()

	// A failed sweep is cached alongside a successful one. Re-running a call
	// that just failed, three times per scrape, would only multiply whatever
	// made it fail.
	if !s.at.IsZero() && time.Since(s.at) < ttl {
		return s.cpus, s.err
	}

	s.cpus, s.err = perfstat.CpuStat()
	s.at = time.Now()
	return s.cpus, s.err
}
