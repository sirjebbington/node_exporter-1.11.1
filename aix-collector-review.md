# AIX Custom Collector Review — Issues Found and Fixed

**Date:** 2026-08-06/07
**Scope:** the ten custom collector files added to this fork on top of upstream node_exporter 1.11.1.
**Status:** all changes applied, type-checked for `GOOS=aix GOARCH=ppc64`, and behaviour-tested against captured real-host output. **Not yet built with CGo on the AIX build server, and not yet deployed.**

Files reviewed:

| File | Collector(s) | Outcome |
|---|---|---|
| `collector/process_aix.go` | `aix_process` | rewritten |
| `collector/srcstatus_aix.go` | `aix_srcstatus` | rewritten |
| `collector/lspath_aix.go` | `aix_lspath` | corrected |
| `collector/diskqueues_aix.go` | `aix_diskqueues` | rewritten (perfstat) |
| `collector/hypervisor_aix.go` | `aix_hypervisor` | rewritten (perfstat) |
| `collector/scheduler_aix.go` | `aix_scheduler` | rewritten (perfstat) |
| `collector/timebase_aix.go` | `aix_timebase` | rewritten (perfstat) |
| `collector/netadapter_aix.go` | `aix_netadapter` | rewritten (perfstat) |
| `collector/fs_nfs_aix.go` | `aix_fsinfo`, `aix_vglv`, `aix_vmstat_fsbuf`, `aix_nfsstat` | unchanged (already remediated — see `rca.md`) |
| `collector/timeout_guard.go` | — | formatting only |
| `collector/perfstat_snapshot_aix.go` | — | **new** shared per-CPU snapshot |

---

## 1. The headline finding: both collectors reported `0` during the `fs_nfs_aix` outages

This is the issue that prompted the review, and it is the most operationally severe one in the set.

During the outages analysed in `rca.md`, `node_src_subsystem_up` and `node_process_up` read **0** for every monitored subsystem and daemon — not a gap. `cron`, `sshd`, `syslogd` and `errdemon` were running normally throughout.

**Neither value came from a measurement. Both collectors manufactured them.**

`process_aix.go`, before:

```go
found, err := c.collectProcesses()
if err != nil {
    // Emit 0 for all targets so dashboards show down rather than missing.
    for _, t := range c.targets {
        ch <- prometheus.MustNewConstMetric(c.descUp, prometheus.GaugeValue, 0, t)
        ch <- prometheus.MustNewConstMetric(c.descPID, prometheus.GaugeValue, 0, t)
    }
    return nil
}
```

`srcstatus_aix.go`, before: in the default `bulk` mode a failed `lssrc -a` left the `bulk` map nil, so every monitored subsystem fell through to the loop's tail and was emitted as `srcRow{Group: "unknown", PID: 0, Status: "absent"}` — `up=0`.

### Why the outage triggered it

The host was slow — SAN contention plus the goroutine/pipe leak of `rca.md` §2d. `ps -ef` and `lssrc -a` blew their 3 s timeouts. Each collector caught the error and, by design, published "everything is down".

It compounded: neither collector set `cmd.WaitDelay`, so **every** timed-out fork permanently stranded two goroutines and a pipe descriptor pair — the exact mechanism of `rca.md` §2d, which had only been fixed in `fs_nfs_aix.go`. `rca.md` §8.2 flagged these files as still carrying it; they were not in the working tree at the time.

### This is a different failure from `rca.md` §4

Two distinct signatures were visible in the same incidents, and they should not be conflated:

| Signature | Cause | What Prometheus stored |
|---|---|---|
| **All series gap, `up=0`** | Whole scrape overran `scrape_timeout` (`rca.md` §4 — one shared `WaitGroup` gates every collector) | nothing |
| **`node_src_subsystem_up == 0`, `node_process_up == 0`, everything else normal** | These two collectors fabricating zeros | a false "service down" |

The second is worse, because it is indistinguishable from a real outage and pages the on-call.

### Fixed

Both collectors now return an error and emit nothing when the command fails. The failure remains visible as `node_scrape_collector_success{collector="aix_process"} == 0`.

Every remaining path that still emits `up=0` now means the daemon genuinely is not there:

| Collector | Situation | Emits |
|---|---|---|
| `aix_process` | `ps` fails or times out (both forms) | **nothing** + collector error |
| `aix_process` | `ps` OK, target not among the processes | `up=0, pid=0, instances=0` |
| `aix_srcstatus` (`bulk`) | `lssrc -a` fails or times out | **nothing** + collector error |
| `aix_srcstatus` (`bulk`) | `lssrc -a` OK, subsystem not listed | `up=0, status="absent"` |
| `aix_srcstatus` (`single`/`auto`) | `lssrc -s` reports `0513-085 ... not on file` | `up=0, status="absent"` |
| `aix_srcstatus` (`single`/`auto`) | `lssrc -s` fails any other way (e.g. `srcmstr` unresponsive) | **nothing** + collector error |

The last row was closed during this review: previously *any* non-timeout `lssrc -s` failure mapped to `absent`, so an unresponsive SRC master reported every subsystem down.

### Alerting change required

`node_process_up == 0` and `node_src_subsystem_up == 0` no longer fire on exporter-side failures. Add a companion alert so those failures stay visible:

```promql
node_scrape_collector_success{collector=~"aix_process|aix_srcstatus"} == 0
```

Panels that previously drew a continuous line at 0 through an outage will now show a gap.

---

## 2. `aix_process` — false positives

### 2.1 Matching included command *arguments*

```go
fields := strings.Fields(line)
if len(fields) < 8 { continue }
cmd := ""
for _, f := range fields[7:] {      // <-- CMD *and every argument*
    if targetSet[f] { cmd = f; break }
}
```

`ps -ef` field 7 onward is the command **plus its whole argument vector**. Any unrelated process that merely *mentioned* a monitored path reported that target as up, with the impostor's PID.

Real triggers: `grep /usr/sbin/cron /etc/inittab`, a wrapper invoked as `/usr/bin/ksh /opt/agent/wrap.sh /usr/sbin/cron`, an admin's `vi /usr/lib/errdemon`, any monitoring script referencing the path.

Reproduced against real `ps -ef` output — the old matcher reported `/usr/sbin/cron` as up with PID `8888888`, the ksh wrapper's PID, while cron was not running. The new matcher reports it absent.

**Fixed:** matching considers only `argv[0]`.

### 2.2 The CMD column could not be located reliably

The scan started at index 7 because AIX's `STIME` column is *one* token for a process started today (`18:25:36`) but *two* for an older one (`Jul 29`), shifting `CMD` between index 7 and 8. Scanning to end-of-line was the workaround — and the direct cause of 2.1.

**Fixed:** the `TIME` column is used as a landmark. It always matches `^[0-9]+:[0-9]{2}$`, which the `HH:MM:SS` form of `STIME` deliberately does not. `CMD` is the token after it. Verified against both shapes from the captured host.

Primary collection now uses `ps -eo pid=,args=` — two columns, no positional guessing at all — with `ps -ef` retained as a fallback for AIX levels that reject `-o`, or that accept it but print something unparseable.

### 2.3 Loose PID parsing

`fmt.Sscanf(fields[1], "%d", &pid)` discarded its error and accepts trailing garbage (`"12abc"` → `12`). Replaced with `strconv.Atoi`, whose failure is what now identifies and skips header rows.

### 2.4 Bare-name targets were unmatchable

Only exact full-path matches worked, so a daemon started via a relative path or symlink was invisible. Targets containing `/` still match the full path; targets without one now match the executable's base name.

### 2.5 Added: `node_process_instances`

New gauge, counting matching processes per target. Detects the duplicate-daemon case that `up`/`pid` cannot express.

---

## 3. `aix_srcstatus` — parsing defects

`lssrc` blank-pads its columns rather than filling them, and **both `Group` and `PID` are optional**. A row therefore carries two to four whitespace-separated fields. The old parser split on whitespace and read fixed offsets, which mis-parses two of the four shapes.

From the captured `lssrc -a` output of a real AIX 7.3 host:

```
Subsystem         Group            PID          Status
 syslogd          ras              10223970     active        <- group + pid   (4 fields)
 qdaemon          spooler                       inoperative   <- group only    (3 fields)
 aso                               9306448      active        <- pid only      (3 fields)
 cdromd                                         inoperative   <- neither       (2 fields)
```

### 3.1 Groupless + running: PID reported as `0`, PID used as the group label

`aso 9306448 active` split into three fields and was read positionally as `name/group/status` — so the **PID became the group label** and `node_src_subsystem_pid` reported **0** for a running subsystem.

Worse, the `group` label then changed on every restart, so **each restart silently started a new time series**, orphaning the old one.

Affected 4 of the 21 active subsystems on the reference host: `aso`, `gc-agent`, `ds_agent`, `node_exporter_aix_go`.

### 3.2 Groupless + stopped: row dropped entirely

`cdromd inoperative` yields two fields; the parser required three and rejected it. The subsystem then fell through to the "not found" tail and was reported `status="absent"` rather than `"inoperative"`. On the reference host this affected 11 subsystems including `isakmpd`, `pnsd`, `nimd`, `gsclvmd`.

### 3.3 Group label churned across state transitions

Combining 3.1 and 3.2: a groupless subsystem going active → inoperative moved from `group="<pid>"` to `group="unknown"` — two different label sets for one subsystem, so range queries and `changes()` broke across the transition. Both shapes now resolve to `group=""`, which is stable in either state.

### 3.4 No status validation — SRC diagnostics became phantom subsystems

Any line with ≥3 fields parsed. SRC's numbered diagnostics (`0513-004 The Subsystem or Group ...`) therefore parsed into subsystems named `0513-004`. Harmless only because the names never collided with a monitored one.

**Fixed:** the last field must be a real SRC state (`active`, `inoperative`, `starting`, `stopping`). This also removes the need for the old `headerSeen` flag, which had its own defect — a blank line set it, so output preceding the header could be accepted.

### 3.5 `lssrc -s` assumed the data row was line 2

```go
if lineNo == 2 { ... }
break
```

Any warning or informational line ahead of the table made a running subsystem read as absent. Now scans for the row whose name matches.

### 3.6 New parser

Anchored on the ends rather than on offsets — the name is always first, the status always last, and what sits between is a PID if numeric and a group otherwise:

| Input | Name | Group | PID | Status |
|---|---|---|---|---|
| `syslogd ras 10223970 active` | `syslogd` | `ras` | 10223970 | active |
| `aso 9306448 active` | `aso` | `""` | 9306448 | active |
| `qdaemon spooler inoperative` | `qdaemon` | `spooler` | 0 | inoperative |
| `cdromd inoperative` | `cdromd` | `""` | 0 | inoperative |
| `Subsystem Group PID Status` | — rejected — | | | |
| `0513-004 The Subsystem ... inoperative.` | — rejected — | | | |

---

## 4. `aix_lspath` — undercounted multipath

Rows were deduplicated on `disk|adapter`. In the plain-`lspath` fallback (no `-F`, therefore no WWN or LUN), a disk reaching several storage ports through one adapter collapsed to a single row — so `node_lspath_disk_total_paths` reported **2 where the host had 4**, on exactly the multipath configurations the metric exists to watch.

The dedup could not simply be removed: `node_lspath_status` is labelled `disk,adapter,wwn,lun,state`, and emitting one label set twice makes the Prometheus registry **reject the entire scrape**.

**Fixed:** dedup retained for the per-path status metric only; the aggregation counters now count every row.

Also fixed:

- **No state validation.** `runCommand` returns combined stdout+stderr, so an `lspath` diagnostic could parse as a path. The state field must now be a known path state.
- `parseFormattedLspath` declared an `error` return it never populated, so the caller's `parseErr` branch was dead. Signature simplified.
- Field alignment was not gofmt-clean.

---

## 5. `aix_diskqueues` — service times were wrong by 1000×

`diskqueues_aix.go` divided libperfstat's `rserv`/`wserv`/`wq_time` by **1e6**. Upstream's `diskstats_aix.go` divides the **same fields** by **1e9**. Both shipped in the same binary, disagreeing by three orders of magnitude on identical data.

**Fixed** by aligning to 1e9, matching upstream — which was validated against real AIX hardware. Nine series change value: `wait_queue_seconds_{total,min,max}`, `read_service_seconds_{total,min,max}`, `write_service_seconds_{total,min,max}`.

**Still worth confirming on a host** (see §9).

---

## 6. Hand-written CGo replaced with the perfstat library

Five collectors (`diskqueues`, `hypervisor`, `scheduler`, `timebase`, `netadapter`) each carried their own CGo preamble doing manual `malloc`, `strcpy` into a `perfstat_id_t`, and `unsafe` pointer arithmetic to walk the record array:

```go
rec := (*C.perfstat_cpu_t)(unsafe.Pointer(uintptr(unsafe.Pointer(buf)) +
    uintptr(i)*uintptr(C.sizeof_perfstat_cpu_t)))
```

`github.com/power-devops/perfstat` is already a direct dependency and is what the default `cpu`, `diskstats`, `netdev`, `filesystem`, `meminfo` and `partition` collectors use. Every field these five needed is exposed by it.

Replacing the CGo removed ~400 lines of memory-unsafe code, three duplicate copies of the same `perfstat_cpu_count`/`fill_first` helper pair, and the `// verify this field name on your header` comments — the field names are now checked by the compiler.

### 6.1 The `cpu` label did not join

The five custom collectors labelled CPUs `cpu="cpu0"`. The default `cpu` collector labels the same logical CPU `cpu="0"` (`cpu_aix.go` uses the slice index). `node_cpu_spurr_ticks_total{cpu="cpu0"}` therefore could not be joined against `node_cpu_seconds_total{cpu="0"}` without `label_replace`.

**Fixed:** unified on the index form, matching the default collector. **Breaking** — see §8.

### 6.2 Three redundant per-CPU sweeps per scrape

`aix_hypervisor`, `aix_scheduler` and `aix_timebase` each called `perfstat_cpu()` independently, walking every logical CPU. With the default `cpu` collector that is four sweeps per scrape; with two Prometheus HA replicas scraping concurrently (`rca.md` §8.3), up to eight.

**Fixed:** new `collector/perfstat_snapshot_aix.go` provides a mutex-guarded, short-TTL shared snapshot with singleflight semantics. New flag `--collector.aix_cpu_snapshot.ttl` (default `2s`, `0` disables). The values are cumulative counters, so reuse within a TTL far below the scrape interval cannot distort a `rate()`.

---

## 7. `cmd.WaitDelay` — `rca.md` §8.2 closed

`rca.md` §8.2 recorded that the `WaitDelay` fix covered only `fs_nfs_aix.go`, and that `lspath`/`process`/`srcstatus` still carried the same goroutine and pipe-descriptor leak — one stranded set per timed-out command, cleared only by restarting the process.

All three now route through the shared `runCommandRaw` helper, which applies `WaitDelay`, the C locale, a full inherited environment, and `%w`-wrapped deadline classification. Verified: `grep -n "exec.Command" collector/*_aix.go` returns exactly one call site, inside `newAIXCommand`.

This also closed a second-order bug — those three collectors previously built their timeout error with `fmt.Errorf` and no `%w`, so `errors.Is(err, context.DeadlineExceeded)` was always false and genuine timeouts were misclassified. Same defect as `rca.md` §7.2(b).

---

## 8. Metric changes before deploy

No metric is renamed or removed. One is added.

| Change | Series affected | Action |
|---|---|---|
| `cpu="cpu0"` → `cpu="0"` | all `aix_hypervisor`, `aix_scheduler`, `aix_timebase` series | update dashboards/alerts filtering on `cpu` |
| Service times ÷1000 | 9 `aix_diskqueues` series (§5) | rescale axes and thresholds |
| `group="<pid>"` → `group=""`, `pid` 0 → real | groupless subsystems only (`aso`, `gc-agent`, `ds_agent`, `node_exporter_aix_go`). Default list `netcd,xntpd,ssh,syslogd` all have groups and are **unaffected** | verify panels |
| `status="absent"` → `"inoperative"` | groupless stopped subsystems | — |
| `node_process_up` values corrected | targets previously matched via arguments | some panels will correctly change 1 → 0 |
| Path counts may increase | `aix_lspath` aggregations, plain-`lspath` hosts only | re-baseline capacity alerts |
| No metrics on command failure | `aix_process`, `aix_srcstatus` | **add the `node_scrape_collector_success` alert from §1** |
| **New:** `node_process_instances{process}` | — | optional panel |
| **New flag:** `--collector.aix_cpu_snapshot.ttl` | — | — |

Unchanged: `aix_fsinfo`, `aix_vglv`, `aix_vmstat_fsbuf`, `aix_nfsstat`, and every default collector.

---

## 9. Verification performed

**Type-check for the target platform.** `GOOS=aix GOARCH=ppc64 go build ./collector/` passes. Since libperfstat's CGo is unavailable off-AIX, this uses the technique from `rca.md` §7.4: a local copy of `power-devops/perfstat` reduced to its pure-Go stubs, wired in via `-modfile` with a `replace` directive, plus `-overlay` for `cpu_aix.go`'s single `sysconf` call. Overlay alone is insufficient — Go refuses to overlay files under `GOMODCACHE`.

This is what confirms every perfstat struct field used actually exists. It does **not** replace a real CGo build on the AIX build server.

**Parser behaviour, 43 assertions, all passing.** The parser functions were extracted *mechanically* from the source (not retyped) into a standalone package and run against verbatim output from `commandoutput10105128.txt`. Coverage: all four `lssrc` row shapes; header and diagnostic rejection; group-label stability across a restart; both `ps -ef` STIME shapes; the false positive reproduced under the old matcher and confirmed absent under the new; `ps -eo` parsing; bare-name matching; multipath counting; `lspath -F` parsing.

**Also clean:** Linux build, `gofmt` across `collector/`, and `go vet` for AIX — the latter reporting only the pre-existing `os_release_test.go` issue already documented in `rca.md` §7.4.

**Pre-existing and unrelated:** 9 Linux collector tests fail on missing fixtures (`fixtures.ttar` is absent from this tree), all in collectors not touched here. Every file changed in this review is `//go:build aix` and excluded from the Linux build.

---

## 10. Open items

1. **Build on the AIX server with CGo.** Nothing here has been compiled by the real toolchain.

2. **Confirm the disk time unit (§5).** On any host: `iostat -D hdisk0` prints `avg serv` in milliseconds; `node_disk_read_service_seconds_total * 1000` should land in the same range. If it does not, the constant is `diskTimeNanoseconds` in `diskqueues_aix.go`.

3. **`aix_netadapter` is now largely redundant.** The default-enabled `netdev` collector reads the same libperfstat table and already exports rx/tx errors and drops as `node_network_*`. Only the interrupt counters are unique. Left in place because removing series is breaking; consider retiring it.

4. **`aix_hypervisor` and `aix_scheduler` partially duplicate the `cpu` collector.** `node_cpu_flags{flag="spurr"}` duplicates `node_cpu_spurr_enabled`, and `node_cpu_context_switches_total` duplicates `node_sched_context_switches_total`. Same recommendation.

5. **Min/max service-time gauges are since-boot extremes.** `node_disk_*_service_seconds_{min,max}` never reset, so they are close to useless for alerting. Consider dropping them.

6. **`rca.md` §8.2 can be marked resolved** (§7 above), and **`findings.md` §3 is still recorded as incorrect** per `rca.md` §8.4 — the `LogicalPartitions` vs PPs mirroring question needs a capture from a host with a mirrored LV. Neither doc was edited in this review.
