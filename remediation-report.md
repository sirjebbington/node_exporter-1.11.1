# AIX Node Exporter — Monitoring Outage: Remediation Report

**Subject:** Loss of server monitoring data on AIX hosts, and the fix now validated in production
**Status:** Fixed. Deployed to one pilot host and confirmed working. Fleet rollout pending.
**Affected monitoring job:** `infra_monitoring_aix_v2`
**Systems affected:** AIX servers including `10.50.4.81` / `10.50.4.82` (Finacle Core Banking DR) and UAT host `custdbdev`

---

## 1. Executive summary

*(Non-technical. This section is self-contained.)*

**What was happening.** Our monitoring system collects health data from each AIX server every 30 seconds. On several servers, that collection progressively slowed down over days until it stopped responding in time. When that happened, we lost **all** monitoring data for that server for that cycle — not just the slow part. Restarting the monitoring agent fixed it immediately, and it then degraded again over the following days.

**Why it mattered.** During the failures we had no visibility into CPU, memory, disk, network or service status on production Core Banking DR servers. The gaps were silent — the dashboards simply showed no data. Two independent monitoring servers watch each host for redundancy; because both were asking the same overloaded agent, **both failed at the same time**, so the redundancy provided no protection.

**What was wrong.** Three separate problems, which compounded each other:

1. **The agent was doing far more work than necessary.** Two of its data collectors ran one AIX command *per filesystem* and *per disk volume*, when a single command returns the same information for all of them at once. On a server with 19 filesystems that meant 22 commands where 2 would do.
2. **It waited in queues it did not need to join.** AIX makes storage commands wait their turn behind any ongoing disk maintenance. The agent was joining that queue on every single request.
3. **It never recovered from a timeout.** This was the hidden cause of the day-by-day worsening. Each time the agent gave up on a slow command, it permanently lost a small amount of internal resource that it could never reclaim while running. The problems accumulated until a restart cleared them.

**What was done.** All three were fixed, plus four data-accuracy defects found during the work (see §6 — two of them meant we were reporting the wrong numbers on an existing dashboard metric).

**Result on the pilot server:**

| Measure | Before | After |
|---|---|---|
| Time to collect storage data | **9 seconds** (the internal limit — it was being hit) | **0.3 seconds** |
| AIX commands run per collection cycle | 22 | 2 |
| Data loss events | Thousands of timeout warnings per day | None observed |

That is a **30× speed improvement**, and the collector is now using roughly 3% of the time budget it was previously exhausting.

**What happens next.** Roll out to the remaining AIX servers in controlled waves. One separate issue remains open and is described in §8 — it is unrelated to this fix and is now the largest remaining risk.

---

## 2. Business impact of the original problem

| Impact | Detail |
|---|---|
| Monitoring blind spots | Complete loss of metrics for affected hosts during failure windows, including production Core Banking DR |
| Redundancy defeated | Both monitoring replicas failed simultaneously because both depend on the same agent on the host |
| Misleading diagnosis risk | Service-status metrics appeared to be broken, prompting investigation of the wrong component. They were collateral damage from a slow, unrelated collector |
| Growing over time | Failures escalated day over day. On host `.81`, timeout warnings went from 2/day to 5,760/day over one week |
| Recovery only by restart | Requiring manual intervention on production servers |

---

## 3. Why the problem existed

### 3.1 Redundant work (the visible cause)

Two collectors re-fetched data they had already retrieved:

- **`aix_fsinfo`** ran `lsfs -q <mountpoint>` once per filesystem. `lsfs -q` with no argument returns the same detail for every filesystem in one call.
- **`aix_vglv`** ran `lsvg -l <vg>` to list logical volumes, then ran `lslv <lv>` again for *each* volume to read two values — both of which were already columns in the output it had just parsed and discarded.

Cost scaled with the size of the server: `3 + N` and `3 + N` commands, where N is the filesystem or volume count. The busiest servers were therefore the worst affected, which matched the observed pattern exactly.

### 3.2 Lock contention (the amplifier)

AIX storage commands take a lock on the volume group and, by default, **wait indefinitely** for it. Any concurrent storage activity — a filesystem resize, a mirror operation, a SAN event — parks the command for as long as that activity runs.

Under quiet conditions each command completed in 10–90 ms. Under load, individual calls were observed taking 1–2 seconds. With 22 commands per cycle, that is the difference between 2 seconds and 30+ seconds. This is why the failures clustered around storage maintenance windows.

### 3.3 A permanent resource leak on every timeout (the escalator)

This is what turned "some slow collections" into "the server degrades until restarted", and it was the least obvious of the three.

The agent stops a command that takes too long. But AIX storage commands are shell scripts that start helper programs, and those helpers inherit the output channel. Killing the script does **not** close that channel, so the agent's internal reader waits forever for an end-of-file signal that never comes.

Every timeout therefore permanently stranded two internal worker threads and a pair of file handles. They were never released while the process ran. This is documented behaviour of the underlying Go language library, and is exactly why the fault escalated daily and cleared completely on restart.

### 3.4 Why one slow collector took down everything

All collectors on a host are gathered behind a single synchronisation barrier — twice over, once in our code and once in the upstream Prometheus library. Nothing is returned until the **slowest** collector finishes. There is also no HTTP-level time limit in the agent.

So a single slow storage collector withheld CPU, memory, network and service-status data too. This is why the outage looked like a total host failure rather than a storage-metrics failure.

---

## 4. What was changed

All changes are in `collector/fs_nfs_aix.go`.

### 4.1 Eliminating redundant commands

| Change | Effect |
|---|---|
| Read PPs and LV state from the `lsvg -l` output already parsed; removed the per-volume `lslv` loop | `3 + N` → `3` commands per volume group |
| Call `lsfs -q` once for all filesystems instead of once each | `3 + N` → `3` commands |
| Removed the redundant plain `lsfs` call — IBM documents `-q` output as a superset of it | `3` → `2` commands |
| Removed a duplicated volume-group listing call that ran twice on the fallback path | 1 fewer command, and removed an unbounded 10-second wait that sat outside the time budget |
| Moved volume-group listing to the `libperfstat` library API | 1 fewer command; no timeout risk at all, as it is a direct library call |

### 4.2 Eliminating the last per-item loop

Reading each filesystem's encryption status still required one command per volume, and AIX offers no batch equivalent. Three layers now sit in front of it:

1. **A 15-minute cache.** Encryption is set when a volume is created; re-reading it every 30 seconds was pure waste.
2. **A volume-group gate.** AIX requires encryption to be enabled at volume-group level before any volume inside it can be encrypted. `hdcryptmgr showvg` (AIX 7.2 TL5+ / 7.3) reports that for every group in a single command. On a server with no encrypted groups — the normal case — this answers every filesystem without running any per-volume command.
3. **A bounded parallel pool** for anything the gate cannot rule out, or for everything if `hdcryptmgr` is unavailable on older AIX, in which case behaviour is unchanged from before.

Measured effect: with one of three volume groups encrypted, per-volume commands dropped from 19 to 3. With none encrypted, from 19 to 0.

### 4.3 Not waiting for storage locks

All `lsvg` and `lslv` calls now pass `-L`, documented by IBM as *"Specifies no waiting to obtain a lock on the Volume group."*

The trade-off, per IBM, is that values may be momentarily inconsistent *while a volume group is actively being modified*. For a monitoring tool that is clearly the right choice: slightly stale partition counts for a few seconds are far better than a stalled collection that discards every other metric on the host.

Reversible without a rebuild via `--collector.aix_vglv.no-lock=false`.

### 4.4 Running volume groups in parallel

Volume groups are independent of one another, so querying them one after another simply multiplied each one's latency by the group count. They now run through a bounded pool (default 4 at a time, `--collector.aix_vglv.concurrency`).

Results are gathered by the workers and emitted by the main routine, so metric ordering stays deterministic and the timing component is only ever touched from a single thread.

### 4.5 Fixing the resource leak

Every command now runs through one shared function that sets a **wait limit** on the output channel. If a helper program holds the channel open after the command is killed, the agent force-closes it after 2 seconds, the reader unblocks, and the resources are returned.

This is the change that stops the day-by-day degradation.

### 4.6 Fixing timeout classification

Timeout errors were being constructed in a way that discarded their cause, so the code that checks *"was this a timeout?"* always answered no. Real command timeouts were silently recorded as generic failures. Corrected so the reported timeout reasons are now accurate.

### 4.7 Correcting the command environment

Commands were being launched with an environment containing only two variables — no `PATH`, no `ODMDIR`, no `LIBPATH`. AIX commands are shell scripts that rely on these. Now inherits the full environment, with the language settings still forced for consistent output parsing.

### 4.8 Adding time limits to two unprotected collectors

`aix_vmstat_fsbuf` and `aix_nfsstat` had no share of the time budget. Both now run under the same limit as the others.

---

## 5. Results

### Measured on the pilot host after deployment

| Metric | Before | After | Change |
|---|---|---|---|
| Storage collector duration | **9.0 s** (limit reached) | **0.3 s** | **30× faster** |
| Budget consumed | 100% (exhausted) | ~3% | 97% headroom recovered |
| Commands per cycle, `aix_fsinfo` | 22 | 2 | −91% |
| Commands per cycle, `aix_vglv` | 11, sequential | 10, 4-way parallel | ~3 rounds instead of 11 |
| Timeout warnings | Thousands per day | None observed | Resolved |

### Original evidence this fixed

From the production logs on host `.81`, timeout warnings escalated 2 → 1,015 → 4,739 → 5,727 → … → 5,760 per day over one week. Of 35,264 total warnings, **28,193 (80%)** carried reasons produced directly by the two redundant loops that were removed.

---

## 6. Data-accuracy defects found and corrected

Separate from performance. These were producing wrong values.

| # | Defect | Consequence |
|---|---|---|
| 1 | The `vmstat -v` output patterns read the number from the *following* line | `node_jfs2_fsbuf_blocked_total` was reporting the **client** counter and `node_jfs2_client_fsbuf_blocked_total` was reporting the **external pager** counter. **These two values change after deploy — see §7** |
| 2 | AIX error messages begin with a device path, identical in form to a data row, and were parsed as filesystems | Each unreadable device created a phantom filesystem entry. Harmless only by luck — a differently-worded error message would have produced bogus metrics |
| 3 | A fallback parser read the wrong column as volume state | A volume without a mount point column reported its disk count as its state |
| 4 | A library value counting *logical* partitions was substituted for one counting *physical* partitions | On a **mirrored** volume the two differ by the copy count — a mirrored `rootvg` would have reported half its true partition usage. Reverted to the authoritative source |

Defect 4 is notable: the earlier analysis validated the substitution against 85 volumes on a real server and found an exact match. It only matched because **none of those volumes were mirrored**, so the two values could not diverge. The internal `findings.md` document should be amended accordingly.

A new metric, `node_jfs2_external_pager_fsbuf_blocked_total`, was added at no extra cost — it is the counter that actually corresponds to JFS2, which is what our fleet uses.

---

## 7. Rollout: what to watch

### Expected, not a fault

`node_jfs2_fsbuf_blocked_total` and `node_jfs2_client_fsbuf_blocked_total` will **step to different values** on each upgraded host (defect #1 above). Because these are counters, the correction will appear as a one-off spike in any rate-based dashboard or alert.

**Action before the next wave:** check whether any alert rule references these two metrics.

### Validation queries per host

```promql
# 1. Collectors succeeding
node_scrape_collector_success{instance=~"$HOST:.*", collector=~"aix_vglv|aix_fsinfo"}

# 2. Timeouts gone — absent is the pass condition
node_scrape_collector_timeout{instance=~"$HOST:.*"}

# 3. Duration, same host before vs after
node_scrape_collector_duration_seconds{instance=~"$HOST:.*", collector=~"aix_vglv|aix_fsinfo"}

# 4. The leak fix — must stay flat over days, unlike unpatched hosts
go_goroutines{job="infra_monitoring_aix_v2"}
process_open_fds{job="infra_monitoring_aix_v2"}

# 5. Orphaned storage configuration surfaced by the new metric
node_filesystem_stale_info{instance=~"$HOST:.*"}
```

Item 4 is the one that requires patience — the leak fix can only be proven by a **flat trend over several days** on the patched host compared with a rising trend on the others.

### Reversible without rebuild

- `--collector.aix_vglv.no-lock=false` — restore lock-waiting behaviour
- `--collector.aix_vglv.concurrency=1` — restore sequential volume-group queries
- `--collector.aix_fsinfo.encryption-cache-ttl=0` — disable encryption caching

### Useful by-product

`node_filesystem_stale_info` reports filesystems listed in `/etc/filesystems` whose storage no longer exists — orphaned configuration. On the reference server this identified **7** such entries. Worth passing to the Unix team for cleanup.

---

## 8. Open items

| # | Item | Priority |
|---|---|---|
| 1 | **The `filesystem` collector has no time limit at all** and was measured at **24.2 seconds** on host `.81` during the incident. It uses a library call that cannot be interrupted the same way, so it needs a different approach (caching or a watchdog). With the storage collectors now fixed, **this is the largest remaining risk** to the collection window | High |
| 2 | **The leak fix must be applied to the other AIX collectors.** The source tree used for this work contains only 4 of the 11 AIX collectors. The others (`aix_srcstatus`, `aix_lspath`, `aix_process`, and five more) also run AIX commands and carry the same defect | High |
| 3 | Amend `findings.md` §3 per defect #4. To settle it fully, capture the comparison on a server with a **mirrored** volume | Medium |
| 4 | Confirm the encryption field is readable via `lslv` on AIX 7.3. IBM's documented interface is `hdcryptmgr showlv`; if `lslv` does not report it, the encryption metric has always read zero regardless of truth. One-line check: `lslv hd4 \| grep -i encrypt` | Medium |
| 5 | Both monitoring replicas scrape every host on the same schedule, doubling the load. Does not cause the fault but removes the benefit of redundancy when it occurs | Low / architectural |
| 6 | The collection timeout was recently tightened from 50s to 30s fleet-wide, reducing the margin for slow collectors | Informational |

---

## 9. How the fix was verified

Testing was constrained by the fact that this software targets AIX on POWER hardware and cannot be run on a normal build machine. Verification was therefore layered:

| Method | What it proved |
|---|---|
| **Real production output as test fixtures** | Verbatim `lsfs -q`, `lsvg -l`, `lsvg -o` and `libperfstat` output captured from a live AIX 7.3 server was used as test input. The parsers reproduce the server's true state exactly: 27 filesystem records — 19 usable, 7 unreadable, 1 excluded — and 79 logical volume records across 5 volume groups |
| **Leak reproduced, then proven fixed** | A command reproducing the AIX script-plus-helper behaviour was run under a 300 ms limit. Before the fix the agent was **still blocked 4 seconds later**. After, it returns in ~2.3 s and leaks nothing across repeated runs |
| **Concurrent-collection testing** | Because two monitoring replicas query each host simultaneously, all shared state was tested under Go's race detector with two overlapping collections on the same instance, using real command execution and command counting. No races; 50 collections cost 2 commands instead of 50 |
| **Compilation for the AIX target** | The full collector package now type-checks for `AIX/ppc64` on a normal machine — previously believed impossible — giving early detection of errors before reaching the AIX build server |
| **Production deployment** | Confirmed working on the pilot host: 9 s → 0.3 s |

---

## Appendix A — Metrics produced

### `aix_vglv`

| Metric | Labels | Meaning |
|---|---|---|
| `node_vg_free_pps_total` | `vg` | Free physical partitions |
| `node_vg_total_pps` | `vg` | Total physical partitions |
| `node_lv_pps_total` | `lv`, `mountpoint` | Partitions used by the logical volume |
| `node_lv_state` | `mountpoint`, `state` | State, e.g. `open/syncd`, `closed/syncd`, `open/stale` |

### `aix_fsinfo`

| Metric | Labels | Meaning |
|---|---|---|
| `node_filesystem_block_size_bytes` | `mountpoint` | Block size |
| `node_filesystem_inline_log_enabled` | `mountpoint` | 1 = inline journal |
| `node_filesystem_log_device_info` | `mountpoint`, `logdev` | `INLINE`, `/dev/hd8`, or `UNKNOWN` |
| `node_filesystem_sparse_files_enabled` | `mountpoint` | 1/0 |
| `node_filesystem_quota_enabled` | `mountpoint` | 1/0 |
| `node_filesystem_encryption_enabled` | `mountpoint` | 1/0 |
| `node_filesystem_stale_info` | `mountpoint`, `device` | **New** — storage referenced but unreadable |

### `aix_vmstat_fsbuf`

`node_jfs2_fsbuf_blocked_total`, `node_jfs2_client_fsbuf_blocked_total`, `node_jfs2_external_pager_fsbuf_blocked_total` *(new)*

### Filesystem type

Not exported by `aix_fsinfo`. It is available from the standard `filesystem` collector as the `fstype` label on `node_filesystem_size_bytes` (`jfs2`, `jfs`, `nfs`, `cdrfs`, …), joinable on `mountpoint`.

---

## Appendix B — New configuration flags

| Flag | Default | Purpose |
|---|---|---|
| `--collector.aix_vglv.no-lock` | `true` | Do not wait for volume-group locks |
| `--collector.aix_vglv.concurrency` | `4` | Volume groups queried in parallel |
| `--collector.aix_fsinfo.encryption-cache-ttl` | `15m` | Encryption status cache lifetime; `0` disables |

---

## Appendix C — Code-level detail

*(Technical overview. Line numbers "before" refer to the pre-change `collector/fs_nfs_aix.go`, 1,029 lines; "after" to the current file, 1,455 lines.)*

### C.0 Index

| # | Defect | Before | After | Class |
|---|---|---|---|---|
| 1 | No `WaitDelay` — permanent goroutine/FD leak on every timeout | `:58-71` | `:94-131` | Leak |
| 2 | Timeout error not wrapped — `errors.Is` always false | `:65` | `:121-123` | Wrong classification |
| 3 | Two-variable command environment (no `PATH`/`ODMDIR`) | `:62` | `:100` | Fragility |
| 4 | Duplicate `lsvg -o` — two forks, one outside the budget | `:556-579` | `:941-966` | Waste |
| 5 | Per-VG loop strictly sequential | `:589-646` | `:968-1006` | Latency |
| 6 | No `-L` — every LVM call waits on the ODM lock | `:594`, `:615` | `:163-175` | Latency |
| 7 | Redundant plain `lsfs` call | `:233` | removed | Waste |
| 8 | `lsfs` diagnostics parsed as filesystem rows | `:348` | `:476-489` | Wrong data |
| 9 | `vmstat -v` patterns read the *next* line's number | `:800-801` | `:1195-1197` | Wrong data |
| 10 | `lsvg -l` fallback used PVs count as the LV state |  `:1128` | `:1128` | Wrong data |
| 11 | perfstat logical partitions overrode physical PPs | `:624-630` | `:887-893` | Wrong data |
| 12 | Regexes recompiled per call / per VG | `:126-133`, `:730` | `:983-986`, `:1110` | Waste |
| 13 | `vmstat`/`nfsstat` ran outside the time budget | `:796`, `:850`, `:862` | guarded | Unbounded |

---

### C.1 The leak, error wrapping and environment — all in one 14-line function

**Before** (`:57-71`) — the single most consequential block in the file:

```go
func runCmdCtx(timeout time.Duration, name string, args ...string) ([]byte, error) {
    ctx, cancel := context.WithTimeout(context.Background(), timeout)
    defer cancel()
    cmd := exec.CommandContext(ctx, name, args...)
    cmd.Env = append(cmd.Env, "LC_ALL=C", "LANG=C")   // (3) cmd.Env is nil -> 2 vars total
    out, err := cmd.CombinedOutput()                  // (1) blocks forever if a helper holds the pipe
    if ctx.Err() == context.DeadlineExceeded {
        return nil, fmt.Errorf("%s %v timed out after %s", name, args, timeout)  // (2) no %w
    }
    ...
}
```

Three defects in five lines:

**(1) The leak.** `cmd.WaitDelay` is never set. Go's `os/exec` documents the consequence verbatim:

> If WaitDelay is zero (the default), I/O pipes will be read until EOF, **which might not occur until orphaned subprocesses of the command have also closed their descriptors for the pipes.**

`lsvg`/`lsfs`/`lslv` are Korn shell scripts that fork helpers (`lqueryvg`, `getlvodm`). Those helpers inherit the stdout pipe. The context deadline kills the *script*; the inherited write end stays open; `io.Copy` never sees EOF; `CombinedOutput` never returns. **Two goroutines and one pipe pair stranded per timeout, unrecoverable until restart.**

**(2) `fmt.Errorf` without `%w`** discards the wrapped cause, so `errors.Is(err, context.DeadlineExceeded)` at every call site was **always false**. Real command timeouts fell through to a generic `Debug` log. Every `*_timeout` label in the production logs came only from the pre-check path.

**(3) `append(cmd.Env, ...)` on a nil `Env`** yields a 2-element environment — no `PATH`, no `ODMDIR`, no `LIBPATH` for the ksh wrappers.

**After** (`:94-131`) — one shared entry point for every command in the file:

```go
func newAIXCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
    cmd := exec.CommandContext(ctx, name, args...)
    cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")   // (3) inherit, then force C locale
    cmd.WaitDelay = cmdWaitDelay                           // (1) 2s — bounds Wait unconditionally
    return cmd
}

func runCommandRaw(ctx context.Context, name string, args ...string) ([]byte, error) {
    ...
    if cerr := ctx.Err(); cerr != nil {
        return out, fmt.Errorf("%s %v timed out: %w", name, args, cerr)   // (2) %w
    }
    if errors.Is(err, exec.ErrWaitDelay) {
        return out, nil   // helper held the pipe; output is complete, leak contained
    }
    ...
}
```

---

### C.2 Volume group listing forked `lsvg -o` twice

**Before** (`:556-579`):

```go
vgList, err := listVGsPerfstat()
if err != nil {
    vgList, err = listVGs()          // runs `lsvg -o` via UNGUARDED runCmd -> fixed 10s,
}                                    // entirely outside the 9s collector budget
if err != nil {
    out, lerr := runCmdWithGuard(guard, "lsvg", "-o")   // runs `lsvg -o` AGAIN
    ...
}
```

Two forks of the same command on the fallback path, the first able to consume 10 s of a 9 s budget before the guard ever saw it.

**After** (`:941-966`) — one guarded call, and an empty perfstat result now also falls back (every AIX host has at least `rootvg` varied on, so empty means the library returned nothing useful):

```go
if vgs, err := listVGsPerfstat(); err != nil {
    c.logger.Debug("perfstat VolumeGroupStat failed; falling back to lsvg -o", "err", err)
} else if len(vgs) > 0 {
    return vgs, nil
}
out, err := runCommand(ctx, "lsvg", lsvgArgs("-o")...)
```

---

### C.3 Sequential per-VG loop, with every call waiting on the ODM lock

**Before** (`:589-646`) — each VG's two commands run back to back, and each one waits for the lock:

```go
for _, vg := range vgList {
    if !guard.MustWithinBudget("lsvg_budget_exhausted") {
        break                                        // silently drops the remaining VGs
    }
    out, err := runCmdWithGuard(guard, "lsvg", vg)        // no -L: waits for the lock
    ...
    lvOut, err := runCmdWithGuard(guard, "lsvg", "-l", vg) // no -L: waits again
    ...
}
```

Total latency was `sum of every call`, so one contended VG delayed all the others. This is the loop that produced 6,732 `lsvg_budget_exhausted` warnings on host `.81`.

**After** — `-L` on every call (`:163-175`), and the VGs fan out over a bounded pool (`:968-1006`):

```go
func lsvgArgs(args ...string) []string {
    if *aixLVMNoLock {
        return append([]string{"-L"}, args...)   // "no waiting to obtain a lock" (IBM)
    }
    return args
}
```

```go
for i := 0; i < min(workers, len(vgs)); i++ {
    go func() {
        defer wg.Done()
        for vg := range jobs {
            r := c.queryVolumeGroup(ctx, vg)
            mu.Lock(); out[vg] = r; mu.Unlock()
        }
    }()
}
```

Workers **gather**; the main routine **emits**. That keeps metric ordering deterministic and means `TimeoutGuard` — which is not safe for concurrent use — is only ever touched from the collector's own goroutine.

---

### C.4 `lsfs` diagnostics parsed as filesystems

**Before** (`:346-356`) — any line beginning `/dev/` was treated as a table row:

```go
if strings.HasPrefix(line, "/dev/") {
    current = fsInfo{}
    parts := strings.Fields(line)
    if len(parts) >= 3 {
        current.dev = parts[0]
        current.mp  = parts[2]
    }
    continue
}
```

But `lsfs` interleaves its per-device errors, and **they also begin with `/dev/`**:

```
/dev/spot1      --         /spot1     jfs2  --   --   yes  no     <- real row
/dev/spot1: A file or directory in the path name does not exist.  <- ALSO matches
The volume group of /dev/spot1  may be varied off.
```

Each unreadable device produced a phantom entry with `dev="/dev/spot1:"`, `mp="file"`, `vfs="or"` — the third and fourth *words of the English error message*. On the reference server this turned 27 real rows into 34. They were discarded downstream only because `"or"` happens not to equal `jfs2`.

**After** (`:476-489`):

```go
func isLsfsTableRow(line string) ([]string, bool) {
    if !strings.HasPrefix(line, "/dev/") {
        return nil, false
    }
    parts := strings.Fields(line)
    // A table row's device field is a bare path; "/dev/spot1:" prefixes an error.
    if len(parts) < 4 || strings.HasSuffix(parts[0], ":") {
        return nil, false
    }
    return parts, true
}
```

Verified against the captured output: 34 rows → 27 (19 usable, 7 stale, 1 cdrfs), matching the server exactly.

---

### C.5 `vmstat -v` read the wrong line — wrong values on a live dashboard metric

**Before** (`:800-801`):

```go
fsbuf  := grepInt(out, `(?m)filesystem I/Os blocked with no fsbuf\s+(\d+)`)
client := grepInt(out, `(?m)client filesystem I/Os blocked with no fsbuf\s+(\d+)`)
```

`vmstat -v` prints the **count before the label**, one statistic per line:

```
           5512714  filesystem I/Os blocked with no fsbuf
                 0  client filesystem I/Os blocked with no fsbuf
            194775  external pager filesystem I/Os blocked with no fsbuf
```

Neither pattern is anchored, so `\s+` matched across the newline and `(\d+)` captured the **following line's** number. `node_jfs2_fsbuf_blocked_total` therefore reported the *client* counter, and `node_jfs2_client_fsbuf_blocked_total` reported the *external pager* counter.

**After** (`:1195-1197`) — anchored at both ends so `client`/`external pager` cannot cross-match:

```go
vmstatFSBufRe       = regexp.MustCompile(`(?m)^\s*(\d+)\s+filesystem I/Os blocked with no fsbuf\s*$`)
vmstatClientFSBufRe = regexp.MustCompile(`(?m)^\s*(\d+)\s+client filesystem I/Os blocked with no fsbuf\s*$`)
vmstatExtPagerRe    = regexp.MustCompile(`(?m)^\s*(\d+)\s+external pager filesystem I/Os blocked with no fsbuf\s*$`)
```

This is the change behind the counter step described in §7.

---

### C.6 Two smaller wrong-data defects

**LV state read from the wrong column** (`:750`):

```go
row.state = parts[len(parts)-2]     // counts back from the end
```

`lsvg -l` columns are `LV NAME  TYPE  LPs  PPs  PVs  LV STATE  MOUNT POINT`. On a row with no `MOUNT POINT`, counting back two lands on **PVs** — so the state label became a disk count. Now indexes by documented position (`:1132`).

**Logical vs physical partitions** (`:624-630`):

```go
if perfstatPPs != nil {
    if v, ok := perfstatPPs[perfstatLVKey(vg, row.lv)]; ok {
        pps = v            // unconditional override with LogicalPartitions
    }
}
```

`LogicalPartitions` counts *logical* partitions; the `PPs` column counts *physical* ones. They are equal only when `mirrors == 1`. A mirrored `rootvg` would have reported half its real `node_lv_pps_total`. Now a fallback only (`:887-893`):

```go
if pps == 0 && perfstatPPs != nil {   // 0 means the CLI row failed to parse
    ...
}
```

The earlier validation missed this because all 85 volumes in the reference capture are unmirrored, so the two values could not diverge.

---

### C.7 Waste and unbounded calls

**Regex recompiled on every call** — `grepInt` (`:126-133`) compiled its pattern per invocation, and `parseLsvgDashL` (`:730`) compiled its row pattern inside the function, i.e. once per volume group per scrape. Both now compile once at package level.

**Commands outside the time budget** — `runCmd("vmstat", "-v")` (`:796`), `runCmd("nfsstat", "-s")` (`:850`), `runCmd("nfsstat", "-c")` (`:862`) each had a fixed 10 s timeout with no share of the collector budget; three such calls could add 30 s to a 30 s scrape window. All three now run under a `TimeoutGuard`.

**Redundant `lsfs`** (`:233`) — `runCmdWithGuard(guard, "lsfs")` ran immediately before `lsfs -q`. IBM documents `-q` as printing superblock detail *"in addition to other file system characteristics reported by the lsfs command"*, so the base table is identical. Call removed; stale-device detection now derives from the same `-q` output (a row with no `(…)` attribute line).

---

## Appendix D — Related documents

| Document | Contents |
|---|---|
| `rca.md` | Full root-cause analysis with log evidence, counts and PromQL |
| `reviewer_reference.md` | Engineering record: verified facts, test methods, open items |
| `findings.md` | Library migration analysis — **§3 requires correction, see §6 defect #4** |
