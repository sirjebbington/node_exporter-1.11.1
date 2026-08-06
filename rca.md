# RCA: AIX Node Exporter Scrape Failures — Hosts 10.50.4.81 / 10.50.4.82

**Status:** Root cause identified and fixed in code (pending build/deploy). The goroutine/pipe leak previously listed as an unconfirmed contributing factor has since been **reproduced and fixed** (§2d, §7.2). One structural contributing factor remains open (`filesystem` collector, §8.1).
**Affected job:** `infra_monitoring_aix_v2`
**Primary hosts analyzed:** `10.50.4.81`, `10.50.4.82` (DR, `Finacle (Core Banking)_DR`)
**Also observed on:** UAT host `custdbdev` (same signature, independently confirmed)
**Reference host for command-output validation:** `nimserver` / `10.10.5.128` (AIX 7.3, healthy — used as a negative control, §7.4)

---

## 1. Summary

Node Exporter on AIX hosts `10.50.4.81` and `10.50.4.82` progressively failed to respond to Prometheus scrapes within the job's `scrape_timeout`, causing `up=0` and full metric gaps (across **all** collectors, not just the slow one) for the affected scrape cycles. The failure pattern escalated over days of uptime and cleared immediately on process restart.

Two internal AIX collectors (`aix_vglv`, `aix_fsinfo`) were making far more subprocess calls per scrape than necessary — re-fetching data that was already available from a command they had just run. Under load/contention, these redundant calls pushed the collectors past their internal 9-second budget, and the resulting slow/incomplete scrapes cascaded into full-scrape failures once the process-wide response time approached the job's scrape_timeout.

---

## 2. Root cause

**File:** `node_exporter/collector/fs_nfs_aix.go`

### 2a. `aix_vglv` — redundant per-LV `lslv` calls
For every logical volume in a volume group, the collector called `lsvg -l <vg>` (once, correctly) **and then called `lslv <lv>` again for every single LV** just to read two fields — `PPs` and `LV STATE`. Both fields are already present as columns in the `lsvg -l` output that had just been parsed; the parser was simply discarding them.

- Old cost per VG: `3 + N` subprocess forks (N = LV count)
- Fixed cost per VG: `3` forks, flat, regardless of LV count

### 2b. `aix_fsinfo` — redundant per-filesystem `lsfs -q` calls
The collector called `lsfs -q <mountpoint>` once **per filesystem**. AIX's `lsfs -q` with no argument returns the same attributes for **every** mounted filesystem in a single call — confirmed against real captured output from three independent hosts.

- Old cost: `3 + N` forks (N = filesystem count)
- Fixed cost: `3` forks, flat

### 2c. Contributing: `aix_fsinfo`'s encryption lookup bypassed the shared timeout budget
`lvEncryptionEnabled()` used an unguarded `runCmd` (fixed 10s timeout) instead of the collector's shared `TimeoutGuard` budget — adding unaccounted time on top of the reported 9s budget. It was first moved onto the shared budget and parallelized via a 4-worker bounded pool. It has since been reduced further to a batch VG-level gate — see §7.2.

### 2d. The escalation mechanism: a goroutine and pipe-descriptor leak on every timed-out command
*(Promoted from "unconfirmed hypothesis" — this is now reproduced. See §7.4 for how.)*

None of the `exec.CommandContext` call sites set `cmd.WaitDelay`. Go's `os/exec` documents the consequence directly:

> If WaitDelay is zero (the default), I/O pipes will be read until EOF, **which might not occur until orphaned subprocesses of the command have also closed their descriptors for the pipes.**

AIX's LVM and filesystem commands (`lsvg`, `lsfs`, `lslv`) are Korn shell wrappers that fork helper binaries (`lqueryvg`, `getlvodm`, …). Those helpers inherit the command's stdout/stderr pipe. When the `TimeoutGuard` deadline fires, `exec` kills the **wrapper**, but the inherited write end of the pipe stays open in the helper. `io.Copy` therefore never sees EOF, `Cmd.Wait()` never returns, and **the calling goroutine, the internal copy goroutine, and one pipe descriptor pair leak permanently — one set per timed-out command.**

This is what converts §2a/§2b from "some scrapes are slow" into "the process degrades monotonically over days and only a restart recovers it": every timeout permanently consumes resources, so the deeper the process gets into the failure mode, the fewer resources it has to get out of it.

### Both loops were wrapped in a 9-second `TimeoutGuard` (`collector/timeout_guard.go`) that **silently breaks** once the budget is exhausted — remaining LVs/filesystems for that scrape got no metrics, surfaced only via `node_scrape_collector_timeout{collector,reason}`.

---

## 3. Why this escalates over days instead of failing immediately

- Each redundant call is a fresh AIX ODM lookup. Under quiet conditions each call completes in ~10-90ms (confirmed against real host samples).
- Under load (storage/SAN contention, concurrent LVM activity, patching windows), per-call latency was observed to creep toward 1-2s.
- `count of forks × per-call latency` is what determines whether the 9s budget is exceeded — so hosts with more LVs/filesystems, or hosts under intermittent load, degrade faster and more severely. This matches the observed per-day escalation (see §5) and explains why the two worst hosts previously identified in `Nodeexporter_logs/` (`10.50.3.222`, `10.50.23.59`) also had the highest LV/filesystem counts.
- **Once a timeout occurs it is not a transient event.** Per §2d, each timed-out command permanently strands two goroutines and a pipe descriptor pair. The resource cost therefore *accumulates* rather than resetting each scrape, which is why the daily warning counts in §5 climb rather than oscillating around a steady state.
- The two effects compound: more forks → more timeouts → more permanently-leaked resources → slower process → more timeouts.
- A restart clears all accumulated goroutines/subprocess state immediately — consistent with "restart fixes it, degrades again over subsequent days" being observed on `.81`, `.82`, and the UAT host `custdbdev`.

**Note on why a healthy host shows no leak.** The leak only triggers when a command is *killed mid-flight*. On a host where every LVM command returns in tens of milliseconds, the deadline never fires, so no descriptors are ever stranded and fd/thread counts stay flat indefinitely. A flat baseline on a fast host is therefore consistent with this mechanism, not evidence against it (§7.4).

---

## 4. Why one slow collector breaks metrics for the *entire* host, not just itself

Confirmed directly in code, at two levels:

1. `node_exporter/collector/collector.go` — `NodeCollector.Collect()` uses **one shared `sync.WaitGroup`** across all enabled collectors per scrape; it blocks on `wg.Wait()` until every collector's `Update()` returns.
2. `vendor/.../prometheus/registry.go` — one level up, the Prometheus client library's `Gather()` does the same again with its own shared wait-group across all registered collectors for that HTTP response.

**Consequence:** if `aix_vglv` or `aix_fsinfo` runs long, no collector's data — including fast, healthy ones like `aix_srcstatus` — is flushed to that scrape's response, because all of it is gated behind the same barrier. This is why SRC subsystem status metrics (`node_src_subsystem_pid`) showed gaps that were originally suspected to be a bug in `aix_srcstatus` itself — they are collateral damage from whichever collector was slowest in that cycle.

There is also no HTTP-level or server-level timeout in this exporter (`node_exporter.go`'s `http.Server{}` has no `ReadTimeout`/`WriteTimeout`; `promhttp.HandlerOpts` sets no `Timeout`). The only ceiling on total scrape duration is Prometheus's own client-side `scrape_timeout`.

---

## 5. Evidence from `Nodeexporter_logs/nodeexporter_81.log` and `nodeexporter_82.log`

Both logs start at process boot on **2026-07-24 22:25:04Z** and run through **2026-08-03**, ending mid-warning with no shutdown/panic/signal recorded (consistent with the process being killed/restarted externally rather than crashing on its own).

**Timeout warning counts escalate day over day** (host `.81`):

| Date | `collector timed out` warnings |
|---|---|
| 2026-07-26 | 2 |
| 2026-07-27 | 1,015 |
| 2026-07-28 | 4,739 |
| 2026-07-29 | 5,727 |
| 2026-07-30 | 4,446 |
| 2026-07-31 | 4,785 |
| 2026-08-01 | 5,753 |
| 2026-08-02 | 5,760 |
| 2026-08-03 (partial) | 3,046 |

Host `.82` shows the identical shape (1,618 → 5,559 → 5,757 → 5,760 → 5,760 → 5,717 → 2,755).

**Timeout reason breakdown, host `.81`:**

| Reason | Count |
|---|---|
| `lsfs_q_budget_exhausted` | 17,754 |
| `lslv_budget_exhausted` | 10,439 |
| `lsvg_budget_exhausted` | 6,732 |
| `lsvg_l_timeout` | 311 |
| `lsfs_q_timeout` | 29 |
| `lslv_timeout` | 6 |
| `lsvg_o_timeout` / `lsvg_timeout` | 1 each |

`lsfs_q_budget_exhausted` and `lslv_budget_exhausted` — the two reasons directly caused by the redundant loops fixed in this change — account for **28,193 of 35,264** total timeout warnings on host `.81` (~80%).

**Incident window, 2026-08-02 ~22:10 and ~22:24:** a wave of `collector timed out` warnings on both `aix_vglv` and `aix_fsinfo` immediately precedes a burst of thousands of `write tcp ...: write: broken pipe` errors (4,679 in the 22:10 burst, 1,812 in the 22:24 burst, on host `.81`). This is node_exporter finally finishing a scrape response after Prometheus had already given up and closed the connection.

**The broken-pipe bursts split almost exactly 50/50 between two remote IPs**, confirming both Prometheus HA replicas failed independently at the same time:
- Host `.81`, 22:10 burst: 2,334 from `10.50.23.10` vs 2,345 from `10.50.23.244` (replica 1 / replica 2 of `prometheus_dr_infra_1`)
- Host `.82`, same incident window: 4,045 from `10.50.23.246` vs 3,832 from `10.50.23.247` (replica 1 / replica 2 of `prometheus_dr_infra_3`)

Both hosts failed in the same rough time window despite being scraped by different infra instances — suggesting a shared triggering condition (storage/SAN event, backup/patching window) at that time, in addition to the standing structural cause.

---

## 6. PromQL queries to demonstrate the outage on Thanos

**a) Confirm the collectors were hitting their timeout budget:**
```promql
node_scrape_collector_timeout{job="infra_monitoring_aix_v2", instance=~"10.50.4.81:.*|10.50.4.82:.*"}
```

**b) Confirm scrape duration for the affected collectors approached/exceeded budget:**
```promql
node_scrape_collector_duration_seconds{job="infra_monitoring_aix_v2", instance=~"10.50.4.81:.*|10.50.4.82:.*", collector=~"aix_vglv|aix_fsinfo|filesystem"}
```

**c) Confirm the target went down (`up=0`) during the incident window — set eval time in UI to just after 22:10 or 22:24 on 2026-08-02:**
```promql
up{job="infra_monitoring_aix_v2", instance=~"10.50.4.81:.*|10.50.4.82:.*"} == 0
```

**d) Catch any downtime within a window rather than only the exact eval instant:**
```promql
min_over_time(up{job="infra_monitoring_aix_v2", instance=~"10.50.4.81:.*|10.50.4.82:.*"}[5m]) == 0
```

**e) Corroborate with the goroutine drop/recovery pattern (proxy for stuck collector goroutines releasing):**
```promql
go_goroutines{job="infra_monitoring_aix_v2", instance=~"10.50.4.81:.*|10.50.4.82:.*"}
```

**f) Confirm collateral impact on an unrelated, healthy collector (SRC subsystem status) during the same window:**
```promql
node_src_subsystem_pid{job="infra_monitoring_aix_v2", instance=~"10.50.4.81:.*|10.50.4.82:.*"}
```

**g) Fleet-wide check — which hosts were down in the same window (used to confirm cross-host correlation):**
```promql
min_over_time(up{job="infra_monitoring_aix_v2"}[5m]) == 0
```

---

## 7. Fix applied

**File:** `collector/fs_nfs_aix.go` (1,455 lines as of this change set)

### 7.1 First change set — removing the redundant fork loops

1. `parseLsvgDashL` now extracts `PPs` and `LV STATE` directly from `lsvg -l <vg>` output (columns were already present, previously discarded). The per-LV `lslv` call in `aix_vglv.Update()` was removed entirely.
2. `aix_fsinfo.Update()` now calls `lsfs -q` once (no mountpoint argument) instead of once per filesystem, and filters results to mounted filesystems locally.
3. `lvEncryptionEnabled()` now runs on the collector's shared time budget (via `context.Context`) instead of an unguarded fixed timeout, and per-filesystem encryption lookups are fanned out through a bounded 4-worker pool (`encryptionLookupPool`).
4. Removed now-dead code paths (`lslv_budget_exhausted`, `lsfs_q_budget_exhausted` reasons no longer reachable) and the now-unused `grepString` helper.
5. Added `runCmdWithGuardTolerant` plus the `node_filesystem_stale_info{mountpoint,device}` metric, because `lsfs -q` exits non-zero as soon as *any* referenced device is stale while still printing correct data for every other filesystem. Confirmed against real output in §7.4.
6. `aix_vglv` VG listing moved to `perfstat.VolumeGroupStat()` (zero forks), filtered on `VariedState == 0`.

### 7.2 Second change set — the leak, LVM lock contention, and the remaining 1:N loop

**a) Goroutine/pipe leak (§2d) — fixed.**
All command execution was consolidated behind a single entry point (`runCommandRaw`) which sets `cmd.WaitDelay = 2s`. `Wait` now force-closes the parent's pipe ends after the delay, the copy goroutine unblocks, and `Wait` always returns. Reproduced before and verified after — see §7.4.

**b) Timeouts were never being classified.**
`runCmdCtx` built its timeout error with `fmt.Errorf` and **no `%w` verb**, so `errors.Is(err, context.DeadlineExceeded)` was *always false* for a genuine command timeout. Every `lsvg_timeout` / `lsvg_l_timeout` in the §5 log breakdown came only from the pre-check path (budget already zero before the call); real command timeouts fell through to a `Debug` log and were silently reclassified as generic failures. All errors now wrap `ctx.Err()` with `%w`.

**c) Commands ran with a two-variable environment.**
`cmd.Env = append(cmd.Env, "LC_ALL=C", "LANG=C")` on a nil `Env` produces exactly those two variables — **no `PATH`, no `ODMDIR`, no `LIBPATH`** for AIX's ksh command wrappers. Now `append(os.Environ(), …)`, with the C locale still forced last.

**d) LVM ODM lock contention — the mechanism behind §3's "latency creeps to 1-2s".**
`lsvg`/`lslv` take a lock on the volume group and, by default, retry until they get it. A concurrent `chfs`/`extendlv`/`mirrorvg`, or a SAN event stalling one, parks the command for as long as the lock is held. Both commands now pass `-L` ("Specifies no waiting to obtain a lock on the Volume group", IBM Commands Reference). Controlled by `--collector.aix_vglv.no-lock` (default `true`); IBM's caveat is that values can be momentarily stale *while a VG is being actively modified*, which is the correct trade for a monitoring exporter given §4.

**e) `aix_vglv` per-VG calls are now parallel.**
Volume groups are independent, so running them back to back only multiplied each call's latency by the VG count against the 9s budget. Now fanned out over a bounded pool (`--collector.aix_vglv.concurrency`, default 4). Workers gather into structs and `Update` emits, so metric ordering stays deterministic and the non-thread-safe `TimeoutGuard` is only touched from the collector's own goroutine.

**f) Removed a duplicate `lsvg -o` fork.**
The listing path called `listVGs()` (unguarded, 10s, *outside* the budget) and then, on failure, ran `lsvg -o` again through the guard. Collapsed to one guarded call. An empty perfstat result now also falls back to the CLI (every AIX host has at least `rootvg` varied on, so empty means the library returned nothing useful).

**g) Removed the redundant plain `lsfs` fork.**
IBM documents `-q` as printing superblock detail *"in addition to other file system characteristics reported by the lsfs command"* — the base table is identical, so the separate `lsfs` call was duplicate work. Stale-device detection now falls out of the same output: a row with no `(…)` attribute line is a device whose superblock could not be read.

**h) The last 1:N fork multiplier in `aix_fsinfo` — the per-LV `lslv` encryption loop.**
Three layers, each falling back cleanly to the one below:
1. **TTL cache** (`--collector.aix_fsinfo.encryption-cache-ttl`, default `15m`). LV encryption is a provisioning-time property.
2. **Volume group gate.** IBM requires the data encryption option to be enabled *at VG level* before it can be enabled on any LV inside it, and `hdcryptmgr showvg` (AIX 7.2 TL5+/7.3) reports that for every VG in a single fork. The LV→VG mapping comes from `perfstat.LogicalVolumeStat()` — zero forks. On a host where no VG is encryption-enabled, this answers every LV without running `lslv` at all.
3. **Bounded `lslv` pool** for whatever the gate cannot rule out, or for everything if `hdcryptmgr` is absent (AIX 7.1 / pre-7.2-TL5) — in which case behaviour is exactly as before.

**i) `aix_vmstat_fsbuf` and `aix_nfsstat` were on unguarded 10s timeouts** outside any budget. Both now run under a `TimeoutGuard`, adding reasons `vmstat_v_timeout`, `nfsstat_s_timeout`, `nfsstat_c_timeout`.

**j) Regexes hoisted to package level.** `grepInt` and `parseLsvgDashL` were recompiling their patterns on every call — i.e. per VG, per scrape.

### 7.3 Correctness defects found while in the file

These are not performance issues; they were producing wrong data.

| # | Defect | Effect before fix |
|---|---|---|
| 1 | `vmstat -v` prints the **counter before its label**, but the patterns were unanchored, so `\s+(\d+)` matched across the newline into the *next* line. | `node_jfs2_fsbuf_blocked_total` reported the **client** counter; `node_jfs2_client_fsbuf_blocked_total` reported the **external pager** counter. Both now anchored `^…$`. **These two series change value after deploy.** |
| 2 | `lsfs` prints its diagnostics inline as `/dev/NAME: message`, which starts with `/dev/` exactly like a table row. | Each stale device produced a phantom filesystem entry whose "VFS type" was the fourth *word of the English error message*. Discarded only because those words happened not to be `jfs2` — luck, not logic. Fixed via `isLsfsTableRow` (device field must not end in `:`). |
| 3 | `parseLsvgDashL`'s column-drift fallback took `parts[len-2]` as the LV state. | A row without a `MOUNT POINT` column reported the **PVs count** as its state label. Now uses the documented column order. |
| 4 | perfstat's `LogicalPartitions` was substituted for `lsvg -l`'s PPs column. | `LogicalPartitions` counts *logical* partitions; PPs counts *physical* ones. They diverge by the copy count on a mirrored LV, so **a mirrored rootvg would have reported half its real `node_lv_pps_total`**. See §7.4 for why the original validation missed this. perfstat is now a fallback used only when the CLI row fails to parse. |

**Metric changes to be aware of before deploy:**
- `node_jfs2_fsbuf_blocked_total` and `node_jfs2_client_fsbuf_blocked_total` will report different (correct) values — check any dashboard or alert baselined on them.
- New: `node_jfs2_external_pager_fsbuf_blocked_total`. This is the counter that actually corresponds to JFS2; the plain `fsbuf` counter is JFS. Free from output already being parsed.
- New flags: `--collector.aix_vglv.no-lock`, `--collector.aix_vglv.concurrency`, `--collector.aix_fsinfo.encryption-cache-ttl`.
- Timeout `reason` label set is now: `mount_timeout`, `lsfs_q_timeout`, `lsvg_o_timeout`, `lsvg_budget_exhausted`, `vmstat_v_timeout`, `nfsstat_s_timeout`, `nfsstat_c_timeout`. `lsfs_timeout` is gone (the plain `lsfs` call no longer exists).

### 7.4 Verification performed

**Against real AIX 7.3 output** — verbatim captures from `nimserver` / `10.10.5.128` (`commandoutput10105128.txt`, `coomandoutputlvvg.txt`), used as test fixtures rather than read by eye:

- `lsfs -q`, full 63-line output: parses to exactly **27 rows = 19 usable + 7 stale + 1 cdrfs**, matching the host. Before defect #2 above was fixed it produced 34. The 7 stale devices (`spot1`, `fslv02`, `infinettestlv`, `test1lv`, `test2lv`, `users1lv`, `userslv`) correspond exactly to the VGs `perfstat` reports as varied off (`infivg`, `test1vg`, `test2vg`) — internally consistent. **This also independently confirms the §7.1(5) claim about `lsfs -q` behaviour on stale devices**, which was previously unverified: the command interleaves per-device errors and still prints complete data for every healthy filesystem.
- All 5 `lsvg -l` outputs (2+2+2+55+18 = 79 rows) parse exactly, including LV types `boot`, `paging`, `sysdump`, `jfs2log`, `jfslog` and old-style `jfs`, and mountpoints `N/A`, `/`, and multi-segment.
- Old-style JFS (`/dev/lv00`, `frag size: 4096`, none of the jfs2 attributes) parses correctly.
- `VariedState == 0` means varied ON: the 5 VGs `lsvg -o` lists are exactly the 5 of 9 that perfstat reports as state 0. Third independent confirmation.
- The `errno=6` visible on the perfstat fetch call is harmless — the Go wrapper only checks `r < 0`, so `VolumeGroupStat()` returns all 9 VGs.
- LVs in varied-off VGs come back from `LogicalVolumeStat()` with an **empty VG name** (`infiloglv`, `infinettestlv`, `state=0`). The encryption VG gate correctly treats `""` as unknown and falls through to `lslv` rather than assuming "not encrypted".

**Why defect #4 was not caught earlier:** `findings.md` §3 concluded `LogicalPartitions` exactly matches the PPs column across ~85 LVs. It does — but **every LV on `nimserver` has `mirrors=1`**. The capture contains no mirrored LV, so the two fields could not diverge. `Nimlv` has `PVs=3` with `mirrors=1` (spread across three disks, not mirrored), which is what makes this easy to miss. `findings.md` §3 should be corrected; see §8.4.

**Leak reproduction and fix (Go-level, on Linux):** a command matching the AIX wrapper/helper shape (`sh -c 'sleep 30 & echo started; sleep 20'`) run under a 300 ms deadline left `CombinedOutput` **still blocked 4 seconds later** with `WaitDelay` unset. With `WaitDelay = 2s` it returns in ~2.3 s and classifies as a deadline. Eight iterations leak zero goroutines. Both directions are pinned by regression tests.

**Concurrent scrapes — tested explicitly, because §8.3 means every host is scraped by two HA replicas at once.**

This is not theoretical in this exporter. `collector.go`'s `initiatedCollectors` caches collector instances **globally**, so the *same* collector pointer serves every scrape, and `promhttp.HandlerOpts{MaxRequestsInFlight: 40}` (the default) does not serialise requests. Two overlapping scrapes therefore call `Update()` concurrently on shared state.

Audit of what is actually shared across scrapes in this file:

| Collector | Cross-scrape mutable state |
|---|---|
| `aix_vglv`, `aix_vmstat_fsbuf`, `aix_nfsstat` | none — descriptors and logger only. Each `Update` builds its own guard, context and result maps. |
| `aix_fsinfo` | the encryption cache (`encByDev`, `vgGate`), introduced by §7.2(h). Guarded by `encMu`. |
| package level | compiled regexps (safe for concurrent use) and flag pointers (read-only after parse). |

Tested with two concurrent `Update` paths on one shared collector, forking **real** processes (stub `hdcryptmgr`/`lslv` on `PATH`) so fork counts are measured rather than assumed. All under `-race`:

| Scenario (19 filesystems, from the nimserver capture) | Result |
|---|---|
| 25 scrape rounds × 2 concurrent replicas | **2** `hdcryptmgr` forks, **0** `lslv`, across all 50 scrapes. No races. Every scrape returns a complete, identical answer. |
| 2 simultaneous *cold-cache* scrapes, gate available | 2 `hdcryptmgr`, 0 `lslv` (serial equivalent: 1 and 0) — **one extra fork per 15 min TTL** |
| 2 simultaneous cold scrapes, `hdcryptmgr` absent (pre-7.2-TL5) | **38** `lslv` vs 19 for a single scrape — the §8.3 doubling, now confined to once per TTL instead of every scrape |
| Gate with 1 of 3 VGs encryption-enabled | 3 `lslv` instead of 19 |
| A VG the gate does not mention | falls through to `lslv` rather than defaulting to "not encrypted" — the property that makes the gate sound, since `hdcryptmgr showvg` prints "VG NAME **/ ID**" and an ID-form row would not match |

Two cache-correctness properties are also pinned: serving a cache hit does **not** refresh the entry's timestamp (otherwise a continuously-scraped host would never expire an entry and never notice a change), and the cache is rebuilt from the current device set each scrape so it cannot grow without bound.

**On the residual doubling:** no in-flight deduplication (singleflight) was added. Before this change set two replicas cost 2 × 19 = 38 `lslv` forks *every 30s scrape*; now the same 38 occur at most once per 15-minute TTL, and on AIX 7.2 TL5+/7.3 the gate reduces it to 2 `hdcryptmgr` forks. Deduplication would save roughly 1 fork per minute in the worst case, in exchange for coupling the two replicas' scrapes to each other — a slow lookup in one would then block the other. Not a good trade here; revisit only if a pre-TL5 host shows budget exhaustion attributable to this.

**libperfstat is documented by IBM as a threadsafe API**, so the concurrent `perfstat.LogicalVolumeStat()` / `VolumeGroupStat()` calls that two replicas produce are safe. IBM service documentation also records a known issue where `PERFSTAT_LOGICALVOLUME` reports a failure when LVs exist on a varied-off VG — which is exactly the `errno=6` visible in the nimserver capture, and is harmless because the Go wrapper checks only the return count.

**Build and static verification:**
- `GOOS=aix GOARCH=ppc64 go build ./collector/` — **clean**. `CLAUDE.md` states this cannot be done off-AIX; it can, by supplying a pure-Go stand-in for the CGo-only perfstat symbols via an alternate `-modfile` plus `-overlay`. This type-checks the real source for the target platform. It does **not** replace a real CGo build on the AIX build server.
- Parser and exec/concurrency test suites (the latter under `-race`) pass; both are `go vet` clean; `gofmt` clean.
- Linux build unaffected — `fs_nfs_aix.go` is `//go:build aix` and is excluded from the Linux build (verified via `go list`). `go vet ./collector/` on Linux passes.
- One caveat on tooling: `go vet` under `GOOS=aix` fails on a **pre-existing, unrelated** issue — `os_release.go` is tagged `!aix` but `os_release_test.go` carries no build tag, so the test references a symbol excluded from that build. Not introduced by this work.

**Still not done:** compilation on a real AIX build server with CGo enabled, and a controlled rollout.

---

## 8. Open items — not yet resolved by this fix

1. **`filesystem` collector has no timeout protection at all — now the largest remaining contributor.** Thanos data showed `node_scrape_collector_duration_seconds{collector="filesystem"}` reaching **24.2 seconds** on host `.81` at the exact incident timestamp (2026-08-02 22:10:12). This collector calls `perfstat.FileSystemStat()` (CGo, not a subprocess) and has no `TimeoutGuard` or equivalent anywhere in `filesystem_aix.go`/`filesystem_common.go`. A 24s stall alone consumes the majority of the job's 30s `scrape_timeout`. With `aix_vglv` and `aix_fsinfo` now bounded and largely fork-free, this is the single biggest remaining risk to the scrape window. Note it cannot be fixed the same way — a CGo call cannot be cancelled by a context; it would need a cached-result / stale-while-refresh pattern or a watchdog goroutine.

2. **The `WaitDelay` fix covers only the collectors present in this working tree.** `fs_nfs_aix.go` is the **only** file here that uses `os/exec` — but this tree contains just 4 of the 11 registered AIX collectors (`aix_fsinfo`, `aix_vglv`, `aix_vmstat_fsbuf`, `aix_nfsstat`). The deployed binary also runs `aix_srcstatus`, `aix_lspath`, `aix_process`, `aix_timebase`, `aix_diskqueues`, `aix_hypervisor`, `aix_netadapter`, `aix_scheduler` — all of which fork subprocesses, and none of whose source files exist here (`srcstatus_aix.go`, `lspath_aix.go`, `process_aix.go`, …). **Those files carry the same leak (§2d) and must receive the same `cmd.WaitDelay` treatment**, ideally by routing them through the shared `runCommandRaw` helper rather than duplicating it. Until that is done the leak is only partially closed.

3. **Two independent Prometheus HA replicas scrape every host on the same schedule.** This does not cause the underlying slowness but doubles subprocess/ODM load on the AIX host per scrape cycle, and guarantees that when a scrape fails, it fails for both replicas' data streams simultaneously rather than one covering for the other.

4. **`findings.md` §3 is now known to be incorrect and should be amended.** It concludes perfstat's `LogicalPartitions` is a safe direct replacement for `lsvg -l`'s PPs column, based on an exact match across ~85 LVs. That evidence is real but insufficient: every LV in the capture has `mirrors=1`, so the two fields could not possibly have diverged. The code has been changed accordingly (§7.3 defect #4). **To settle this properly, capture `lsvg -l` plus the perfstat dump from a host with a mirrored LV** (one where `PPs = 2 × LPs`) and confirm whether `PPs = LogicalPartitions × Mirrors` holds. If it does, the perfstat path can be promoted back to primary with the multiplication applied.

5. **Fleet-wide time correlation at 2026-08-02 ~22:10 and ~22:24** across hosts scraped by different infra instances (`.81` via `prometheus_dr_infra_1`, `.82` via `prometheus_dr_infra_3`) suggests a shared external trigger (SAN/storage event, backup or patching window) worth investigating independently of the structural fix.

6. `infra_monitoring_aix_v2` job's `scrape_timeout` in DR was recently reduced from 50s to 30s (infra2, 2026-07-28) and set to 30s fleet-wide across infra1/2/3 — tightening the margin available for slow collectors like `filesystem` going forward.

7. **`lslv`'s `ENCRYPTION:` field is unconfirmed on AIX 7.3.** `lvEncryptionEnabled()` greps `lslv` output for `^ENCRYPTION:\s*yes`, but IBM's documented interface for LV encryption status is `hdcryptmgr showlv`, and the published `lslv` field list does not include an ENCRYPTION field. If `lslv` does not print it on 7.3, `node_filesystem_encryption_enabled` has been reporting 0 everywhere regardless of the truth. **Quick check on any host: `lslv hd4 | grep -i encrypt`.** If absent, layer 3 of §7.2(h) should switch to `hdcryptmgr showlv`; the VG gate in layer 2 is unaffected either way.

---

## 9. Deployment checklist

1. Build on the AIX build server per `CLAUDE.md` (CGo enabled, `GOOS=aix GOARCH=ppc64`).
2. Confirm `--help` shows the three new flags (§7.2).
3. Roll out to **one** host first, ideally `.81` or `.82`, and watch for one full day:
   - `node_scrape_collector_timeout{collector=~"aix_vglv|aix_fsinfo"}` should drop to near zero.
   - `node_scrape_collector_duration_seconds{collector=~"aix_vglv|aix_fsinfo"}` should fall sharply.
   - `go_goroutines` should stay flat instead of trending upward across days — this is the direct signal that §2d is closed.
   - `node_jfs2_fsbuf_blocked_total` / `node_jfs2_client_fsbuf_blocked_total` will step to new values (§7.3 defect #1). Expected, not a regression.
4. Confirm `node_filesystem_stale_info` appears for hosts with orphaned `/etc/filesystems` entries and hand the list to the Unix team for cleanup.
5. If LVM data ever looks momentarily inconsistent during storage maintenance, `--collector.aix_vglv.no-lock=false` reverts the `-L` behaviour without a rebuild.