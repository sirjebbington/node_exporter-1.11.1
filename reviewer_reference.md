# Reviewer Reference — AIX Node Exporter `aix_vglv` / `aix_fsinfo` Timeout Investigation

**Purpose of this file:** a fact-only record of what has been established, changed, and verified in this
repo across the session, written so the next agent can pick it up cold. Every claim below was checked
against actual file contents, log files, or command output in this repo — not inferred. Where something
is unverified or is a hypothesis, it is labeled as such explicitly. **Before acting on anything below,
re-verify against the current file state** — this file is a snapshot, and other agents may have made
further changes since it was written.

**Validation instructions for the next agent:** every section below tells you exactly which file/line to
open and what to check. Do this before trusting any claim here, especially anything under "Current file
state" and "Fix status" — those are the most likely to have drifted.

---

## 1. Repo/task context

- This repo is a fork of Prometheus Node Exporter targeting IBM AIX ppc64.
- **Layout correction (verified this session):** `CLAUDE.md` says the Go source lives in a
  `node_exporter/` subdirectory. In *this* working copy it does not — the module root **is** the repo
  root (`/home/sai/node_exporter-1.11.1/go.mod`), and collectors are at `collector/*_aix.go`. Paths in
  older sections of this document that begin `node_exporter/collector/` should be read as `collector/`.
- **There is no `vendor/` directory in this working copy.** `CLAUDE.md` documents an offline
  `go build -mod=vendor` build; that will fail here. Dependencies resolve from the module cache
  (`~/go/pkg/mod`) instead. The AIX build server presumably has `vendor/`; this checkout does not.
- **This tree contains only 4 of the 11 registered AIX collectors** (verified:
  `grep registerCollector("aix` over `collector/*.go`) — `aix_fsinfo`, `aix_vglv`, `aix_vmstat_fsbuf`,
  `aix_nfsstat`, all in `fs_nfs_aix.go`. `srcstatus_aix.go`, `lspath_aix.go`, `process_aix.go`,
  `hypervisor_aix.go`, `diskqueues_aix.go`, `netadapter_aix.go`, `scheduler_aix.go`, `timebase_aix.go`
  are **absent**, even though the deployed binary on `nimserver` runs all of them (visible in its
  command line in `commandoutput10105128.txt`). Anything that must be applied fleet-wide — notably the
  `WaitDelay` leak fix — is therefore only half-applied here. See §11.2.
- **Correction: the AIX target CAN be type-checked off-AIX.** The claim that verification is limited to
  `gofmt` is no longer accurate. See §10.4 for the working method — `GOOS=aix GOARCH=ppc64 go build
  ./collector/` now completes cleanly. This type-checks the real source for the target platform; it is
  not a substitute for a CGo build on the AIX build server, which remains required per `CLAUDE.md`.

## 2. Original problem (from log analysis)

**Files analyzed:** `Nodeexporter_logs/nodeexporter_81.log`, `nodeexporter_82.log` (hosts `10.50.4.81`,
`10.50.4.82`), plus `nodeexporter (1).log` (UAT host `custdbdev`), plus per-host subdirectories under
`Nodeexporter_logs/`.

- Both `.81`/`.82` logs start at process boot `2026-07-24T22:25:0x` and run through `2026-08-03`.
- `grep "collector timed out" nodeexporter_81.log | ... | cut -c1-10 | sort | uniq -c` shows daily counts
  escalating: 2 (07-26) → 1,015 → 4,739 → 5,727 → 4,446 → 4,785 → 5,753 → 5,760 → 3,046 (partial, 08-03).
  Host `.82` shows the same shape (1,618 → 5,559 → 5,757 → 5,760 → 5,760 → 5,717 → 2,755).
- Reason breakdown on `.81` (`grep -o 'reason=[a-z_]*' | sort | uniq -c`): `lsfs_q_budget_exhausted`
  17,754; `lslv_budget_exhausted` 10,439; `lsvg_budget_exhausted` 6,732; `lsvg_l_timeout` 311;
  `lsfs_q_timeout` 29; `lslv_timeout` 6; `lsvg_o_timeout`/`lsvg_timeout` 1 each.
- UAT log (`custdbdev`) shows the identical signature: quiet for ~28 days (2-15 timeout
  warnings/day, 2026-07-09 through 07-19), then an escalation cliff (1,384 → 1,907 → 2,412 → 1,029)
  ending with the log going silent mid-warning on 2026-07-23 — no panic/fatal/signal recorded.
- A burst of `write tcp ...: write: broken pipe` errors (`http.go:231`) immediately follows
  `collector timed out` warnings at 2026-08-02 ~22:10 and ~22:24 on host `.81` (4,679 and 1,812 lines
  respectively, confirmed via `sed`/`grep` line-range extraction on the actual log).
- Those broken-pipe bursts split near-50/50 between two remote IPs each time: `.81`'s incident —
  2,334 from `10.50.23.10` vs 2,345 from `10.50.23.244`; `.82`'s incident — 4,045 from `10.50.23.246` vs
  3,832 from `10.50.23.247`.

## 3. Root cause identified in code (verified by reading the file directly)

**File:** `node_exporter/collector/fs_nfs_aix.go`

At the point this was first read (before any fix), the code had:
- `aixVGLVCollector.Update()`: called `lsvg -l <vg>` and then, for every LV row returned, called
  `lslv <lv>` again to read `PPs:` and `LV STATE:` — fields already present as columns in the
  `lsvg -l <vg>` output that had just been parsed. The parser (`parseLsvgDashL`) at that time only
  extracted LV name and mountpoint via a loose regex (`\s+.*\s+`), discarding the PPs/TYPE/PVs/STATE
  columns in between.
- `aixFSInfoCollector.Update()`: called `lsfs -q <mountpoint>` once per filesystem in a loop.
- `lvEncryptionEnabled()` used the unguarded `runCmd` (fixed 10s timeout) instead of the collector's
  shared `TimeoutGuard` budget.
- Both loops were wrapped in a `TimeoutGuard` with a 9-second budget (`defaultCollectorBudget` in
  `timeout_guard.go:12`) that breaks the loop silently on exhaustion, logging
  `collector timed out ... reason=<x>_budget_exhausted` — matching exactly the reason strings found in
  the logs in §2.

**Confirmed independently (AIX command behavior):** `lsvg -l <vg>` returns columns
`LV NAME TYPE LPs PPs PVs "LV STATE" "MOUNT POINT"` for every LV in one call; `lsfs -q` with no
mountpoint argument returns the same per-filesystem `(lv size: ..., block size: ..., ...)` attribute
blocks for every mounted filesystem in one call. Verified against three independent real AIX command
output samples provided during this session (`reference/command output.txt`,
`reference/command output (1).txt`, and inline pasted `lsvg -l rootvg`/`lsvg -l nimvg` output) — all
matched the assumed column layout, including edge cases: LV types `boot`, `paging`, `jfs2log`,
`sysdump`, `jfs`, `jfs2`; mountpoints `N/A`, single-segment, and multi-segment paths
(`/var/adm/ras/livedump`); scheduling policies `parallel` and `striped`.

## 4. Fix applied earlier in this session (by this agent)

Implemented in `fs_nfs_aix.go`:
1. `parseLsvgDashL` rewritten to extract PPs and LV STATE directly from `lsvg -l` output (widened regex
   plus a positional fallback for column drift). `lvRow` struct extended with `pps int64` and
   `state string` fields.
2. `aixVGLVCollector.Update()`'s per-LV `lslv` call removed entirely; PPs/state now read from the
   already-parsed `lsvg -l` row.
3. `aixFSInfoCollector.Update()` changed to call `lsfs -q` once (no mountpoint argument) instead of
   looping per filesystem, filtering the parsed result to mounted filesystems locally.
4. `lvEncryptionEnabled()` changed to accept a `context.Context` and use the shared budget via
   `runCmdWithDeadline` instead of the unguarded `runCmd`.
5. Added `encryptionLookupPool` — a bounded 4-worker pool (`encryptionLookupConcurrency = 4`) fanning out
   per-device `lslv` calls for the `ENCRYPTION:` field, since no batch AIX command exists for that
   attribute (confirmed: `lsfs -q`'s output has no encryption field; only `lslv <lv>` reports it).
6. Removed now-dead code (commented-out old logic, the `grepString` helper which became unused).

**Verification performed at that time (not build verification — see §1):**
- `gofmt -l fs_nfs_aix.go` → clean.
- `GOOS=aix GOARCH=ppc64 CGO_ENABLED=0 go build ./collector/...` → zero errors attributable to
  `fs_nfs_aix.go`; only pre-existing CGo-dependent-file errors (unrelated files, expected).
- Extracted `parseLsvgDashL`/`parseLsfsQ`/`encryptionLookupPool` into a standalone non-AIX-tagged Go
  program and ran with `go run -race` against real captured `lsvg -l`/`lsfs -q` samples plus a synthetic
  40-device concurrency stress test — no data races, correct output, no hangs on zero-budget/empty-list
  edge cases. Scratch files were deleted after (`.claude/tmp/verify*`, not committed).
- Grepped `preprod/` dashboard JSON for the removed timeout `reason` label strings
  (`lslv_budget_exhausted`, `lsfs_q_budget_exhausted`) — no references found, so removing those code
  paths does not break any known dashboard/alert query.

## 5. Changes made by another agent since (verified by reading current file state on this pass)

**The file has been further modified since §4.** Confirmed by direct read of
`node_exporter/collector/fs_nfs_aix.go` (943 lines currently vs. 877 after the §4 fix). Changes found,
verified line-by-line against the current file:

1. **New function `runCmdWithGuardTolerant`** (`fs_nfs_aix.go:88-105`). Per its own doc comment: AIX's
   `lsfs -q` (whole-system call) exits non-zero as soon as any single referenced device is stale (e.g. a
   deleted or varied-off LV still listed in `/etc/filesystems`), even though it still prints complete,
   correct output for every other filesystem to stdout. This function returns the output alongside the
   error on a non-timeout failure, instead of discarding it — addressing a real AIX behavior not
   previously accounted for in the original fix.
2. **`aixFSInfoCollector.Update()` now uses `runCmdWithGuardTolerant` for the `lsfs -q` call**
   (`fs_nfs_aix.go:283`), and on a non-timeout error with non-empty output, logs at Debug level and
   continues parsing rather than failing the whole collector (`fs_nfs_aix.go:284-294`).
3. **New metric `node_filesystem_stale_info{mountpoint,device}`** (`staleDesc`, declared
   `fs_nfs_aix.go:167`, `Desc` built `fs_nfs_aix.go:207-211`), emitted (`fs_nfs_aix.go:308-316`) for any
   mountpoint present in `/etc/filesystems`/`lsfs` output but not successfully read by `lsfs -q` — makes
   stale/orphaned LV config visible instead of silently dropping it. This is a **new metric**, not
   present in the version this agent produced in §4.
4. **Filesystem-listing filter changed from a Size-based skip to a VFS-type filter**
   (`fs_nfs_aix.go:249-266`). Comment explains why: a stale jfs2 device also reports Size `--`,
   identically to a genuinely irrelevant entry (e.g. empty cdrfs CD-ROM drive) — filtering on Size alone
   would have wrongly excluded stale-but-real filesystems from being flagged by item 3. Now filters on
   `vfs != "jfs2" && vfs != "jfs"` instead.
5. **`parseLsfsQ` extended to handle old-style JFS** (`fs_nfs_aix.go:368-372`): recognizes `frag size:`
   (used by VFS type `jfs`) as a fallback for `block size:` (used by `jfs2`), since old-style JFS has no
   sparse files/inline log/quota attributes at all.

**Not yet done (checked directly, not present in current file):**
- No `perfstat.LogicalVolumeStat()` / `perfstat.VolumeGroupStat()` usage found anywhere in
  `collector/*.go` (`grep -rn "perfstat\." fs_nfs_aix.go` → no matches). The library-based optimization
  discussed later in this session (see §7) has **not been implemented**.
- `timeout_guard.go` is unchanged from the version reviewed earlier in this session (still 88 lines,
  `defaultCollectorBudget = 9 * time.Second`, same structure).

**Re-verification performed on this pass:**
- `gofmt -l fs_nfs_aix.go` → clean (current file, post other-agent changes).
- `GOOS=aix GOARCH=ppc64 CGO_ENABLED=0 go build ./collector/...` → same result as §4: zero errors
  attributable to `fs_nfs_aix.go`; only the same pre-existing unrelated CGo-file errors.
- Grepped `preprod/` for `node_filesystem_stale_info` and `runCmdWithGuardTolerant` — no references
  found (new metric name is not yet wired into any known dashboard; not necessarily a problem, just a
  fact to note).

**Not yet verified — flagging explicitly, not asserting:**
- Whether AIX's actual exit-code/stdout behavior for `lsfs -q` with a stale device matches the doc
  comment's description exactly (i.e., non-zero exit + still-valid partial stdout). This was stated as
  the rationale in the code comment by the other agent; this agent has not independently confirmed it
  against a real AIX host output sample. **Next agent should verify this against a real captured
  `lsfs -q` run on a host known to have a stale/deleted LV still referenced in `/etc/filesystems`,
  if one becomes available**, before treating it as fully confirmed.

  > **RESOLVED — see §11.1.** The `nimserver` capture has 7 stale devices and confirms the behaviour
  > exactly. It also exposed a bug this bullet's author could not have anticipated: `lsfs` prints its
  > per-device diagnostics as `/dev/NAME: message`, which the parser was reading as filesystem rows
  > (§10.3 defect #2).

## 6. Broader analysis established this session (facts, with confirmation source noted)

- **Shared-WaitGroup blocking:** confirmed by reading `collector/collector.go` (`NodeCollector.Collect()`
  uses one `sync.WaitGroup` across all enabled collectors, `wg.Wait()` blocks the whole scrape) and the
  vendored `vendor/.../prometheus/registry.go` (`Gather()` does the same one level up). This means a
  slow/stuck collector prevents *all* other collectors' data — including fast/healthy ones — from being
  flushed to that scrape's HTTP response.
- **No HTTP-level timeout in node_exporter itself:** confirmed by reading `node_exporter.go` —
  `promhttp.HandlerOpts{}` never sets `Timeout`; `http.Server{}` is a zero-value struct (no
  `ReadTimeout`/`WriteTimeout`/`IdleTimeout`).
- **`filesystem` collector (cross-platform, `filesystem_aix.go`) has zero timeout protection.** Confirmed
  by reading `filesystem_aix.go` — it calls `perfstat.FileSystemStat()` (CGo, not a subprocess) with no
  `TimeoutGuard`, `context.WithTimeout`, or equivalent anywhere in that file or `filesystem_common.go`.
  A Thanos query the user ran (`node_scrape_collector_duration_seconds{collector="filesystem"}`) showed
  this collector taking **24.2 seconds** on host `.81` at `2026-08-02T22:10:12Z` — this was read directly
  from a screenshot the user provided of the Grafana/Thanos UI tooltip, not independently re-queried by
  this agent.
- **Prometheus HA replica pairs confirmed via a separate repo:**
  `/Users/KMBL365176/IdeaProjects/otlp-project/Prometheus-infra`. An Explore-agent sub-task confirmed
  (citing `config/dr/infra2/configs/prometheus.yml` and `config/dr/infra3/configs/prometheus.yml`):
  - `10.50.23.10`/`10.50.23.244` = replica 1/2 of `prometheus_dr_infra_1`.
  - `10.50.23.246`/`10.50.23.247` = replica 1/2 of `prometheus_dr_infra_3`.
  - Global `scrape_interval: 1m`, global `scrape_timeout: 10s`.
  - Job `infra_monitoring_aix_v2` (via `file_sd_configs` → `targets_aix_v2.json`) has a **job-level
    override `scrape_timeout: 30s`** in DR across infra1/infra2/infra3 (confirmed via
    `git log -p`: infra1 added 30s on 2026-07-28 commit `880c5c9f`; infra2 changed from 50s→30s and
    infra3 added 30s fresh, both in commit `deca504d` same day, author Saimoon Bej, PR 655098 message
    "changing scrape time").
  - **This agent has not independently re-verified these exact commit hashes/line numbers in this pass**
    — they come from a sub-agent's report in an earlier turn of this session, not a direct read by this
    reviewer. Next agent should re-run the equivalent grep/git-log check if this fact is load-bearing for
    a decision.
- **Goroutine/subprocess-leak hypothesis — SUPERSEDED. Now reproduced and fixed; see §10.1.** The
  original basis stands (no `cmd.WaitDelay` at any `exec.CommandContext` call site), and the mechanism
  has since been demonstrated directly in Go rather than argued from documentation. The earlier
  statement that it "has not been confirmed" is no longer current.

## 7. perfstat/library-based optimization — status: IMPLEMENTED since this section was written

> **Superseded — read §10.3 defect #4 before acting on this section.** The VG-listing migration was
> implemented and is correct. The LV PPs migration was implemented and then **partially reverted**: the
> conclusion below (and in `findings.md` §3) that `LogicalPartitions` maps to the PPs count is only true
> for unmirrored LVs. It is now a fallback, not the primary source.
>
> Separately, the "no batch AIX command for encryption" premise below has been superseded by
> `hdcryptmgr showvg` (AIX 7.2 TL5+/7.3), which gates the whole per-LV loop — see §10.2.

*(Original text follows, retained for history.)*


Investigated in this session by reading `vendor/github.com/power-devops/perfstat/lvmstat.go` and
`types_lvm.go` directly:
- `perfstat.LogicalVolumeStat()` and `perfstat.VolumeGroupStat()` exist, are CGo-only (`//go:build aix`),
  make zero subprocess forks.
- `LogicalVolume` struct has `Name`, `VGName`, `State` (raw int, no string mapping in this vendored
  package), `PPsize`, `LogicalPartitions` (maps to PPs count), `Mirrors`, IO counters. **No mountpoint
  field.**
- `VolumeGroup` struct has `Name`, disk/LV counts, `VariedState`, IO counters. **No free/total PPs
  field** — meaning `node_vg_free_pps_total`/`node_vg_total_pps` have no perfstat equivalent.
- Conclusion given to the user: LV PPs count and VG listing could partially migrate to perfstat; VG
  free/total PPs, LV state string, LV-to-mountpoint mapping, and all of `aix_fsinfo`'s fields
  (block size, sparse, inline log, quota, encryption) have no perfstat equivalent and must remain
  CLI-based regardless.
- A scoped implementation prompt was written and handed to the user (not executed by this agent) — see
  chat history for the exact prompt text if needed. **As of this pass, no perfstat usage exists in
  `fs_nfs_aix.go`** (confirmed by grep in §5) — this work has not been started.

## 8. Deliverables already written to disk in this repo

- `/Users/KMBL365176/final-delivery/node-exporter-aix/rca.md` — RCA document (root cause, log evidence
  with exact counts, PromQL queries, fix status, open items). Written by this agent earlier in the
  session; **not re-verified against the current file state in this pass** — if the RCA needs to be
  handed to a human audience after further code changes, re-check its "Fix applied" section (§7 of that
  doc) against the actual current `fs_nfs_aix.go` before distributing.
- `/Users/KMBL365176/final-delivery/node-exporter-aix/diagrams.md` — 5 Mermaid diagrams (before/after
  subprocess flow for both collectors, shared-WaitGroup mechanism, incident sequence diagram, escalation
  timeline). Same caveat: written against the code state at that time, not re-verified against current
  state.

## 9. What the next agent should do before making further changes

*(Sections 1-8 above are the historical record. §§10-12 below describe the current state and supersede
them where they conflict. Line numbers anywhere in §§1-8 are stale — the file is now 1,455 lines.)*

1. Re-read `collector/fs_nfs_aix.go` in full — do not trust any line numbers in this document.
2. Re-run the checks in §10.4 before and after any edit.
3. If touching the perfstat PPs path: read §10.3 defect #4 first. `findings.md` §3's conclusion is
   known-incorrect and the code deliberately no longer follows it.
4. Treat `rca.md` and `diagrams.md` as stale the moment `fs_nfs_aix.go` changes again. `rca.md` has
   been updated to match the current state (§7.1-7.4, §8, §9 of that doc); `diagrams.md` has **not**
   and still describes the §4 code state.

---

## 10. Second change set (this session) — `collector/fs_nfs_aix.go`

All items verified by direct execution, not inference. File is now 1,455 lines.

### 10.1 Goroutine/pipe leak — reproduced, then fixed

**Mechanism, quoting `os/exec`'s own docs** (`/usr/local/go/src/os/exec/exec.go`, `WaitDelay` field):

> If WaitDelay is zero (the default), I/O pipes will be read until EOF, which might not occur until
> orphaned subprocesses of the command have also closed their descriptors for the pipes.

AIX LVM/filesystem commands are ksh wrappers that fork helpers inheriting the stdout pipe. Killing the
wrapper on deadline leaves the pipe open, `io.Copy` never sees EOF, `Cmd.Wait` never returns → the
calling goroutine, the copy goroutine and a pipe descriptor pair leak permanently, one set per timeout.

**Reproduced:** `sh -c 'sleep 30 & echo started; sleep 20'` under a 300 ms context deadline left
`CombinedOutput` **still blocked 4 seconds later**. With `cmd.WaitDelay = 2s` it returns in ~2.3 s and
classifies as a deadline; 8 iterations leak zero goroutines. Both directions pinned by tests.

**Also verified in Go's source** (`Cmd.awaitGoroutines`): on WaitDelay expiry it closes the parent pipe
descriptors and *then* drains `goroutineErr`, so the copy goroutine is guaranteed finished before `Wait`
returns. There is no data race on `CombinedOutput`'s shared buffer.

**Fix:** all execution consolidated behind `runCommandRaw`, which sets `cmd.WaitDelay = cmdWaitDelay`
(2s). `exec.ErrWaitDelay` is treated as success-with-complete-output, not failure.

### 10.2 Other changes (see `rca.md` §7.2 for full rationale)

| Area | Change |
|---|---|
| Error classification | Timeout errors now wrap `ctx.Err()` with `%w`. **They previously did not** — `errors.Is(err, context.DeadlineExceeded)` was always false for a real command timeout, so the `*_timeout` reason labels in the §2 log breakdown only ever came from the pre-check path. |
| Environment | `append(os.Environ(), "LC_ALL=C", "LANG=C")`. Was `append(cmd.Env, …)` on a nil `Env` = a two-variable environment with no `PATH`/`ODMDIR`/`LIBPATH`. |
| LVM locks | `-L` on all `lsvg`/`lslv` (flag `--collector.aix_vglv.no-lock`, default true). IBM: "Specifies no waiting to obtain a lock on the Volume group." |
| `aix_vglv` parallelism | Per-VG calls fanned out over a bounded pool (`--collector.aix_vglv.concurrency`, default 4). Workers gather; `Update` emits, so `TimeoutGuard` (not thread-safe) is only touched from the collector goroutine. |
| `aix_vglv` listing | Removed a duplicate `lsvg -o` fork (an unguarded `listVGs()` *and* a guarded retry). Empty perfstat result now falls back to CLI. |
| `aix_fsinfo` | Dropped the redundant plain `lsfs` fork — `-q` output is a documented superset. |
| `aix_fsinfo` encryption | TTL cache → `hdcryptmgr showvg` VG gate → bounded `lslv` pool. `--collector.aix_fsinfo.encryption-cache-ttl`, default 15m. |
| `aix_vmstat_fsbuf`, `aix_nfsstat` | Now under `TimeoutGuard` (were unguarded 10s). New reasons `vmstat_v_timeout`, `nfsstat_s_timeout`, `nfsstat_c_timeout`. |
| Regexes | Hoisted to package level (were recompiling per VG per scrape). |

Current timeout `reason` label set: `mount_timeout`, `lsfs_q_timeout`, `lsvg_o_timeout`,
`lsvg_budget_exhausted`, `vmstat_v_timeout`, `nfsstat_s_timeout`, `nfsstat_c_timeout`.

Fork cost measured against the `nimserver` capture (19 mounted JFS/JFS2 filesystems, 5 varied-on VGs):
`aix_fsinfo` 22 → 2 forks; `aix_vglv` 11 → 10 forks but fanned 4-wide (~3 rounds instead of 11 serial).

### 10.3 Correctness defects found and fixed

1. **`vmstat -v` counters were off by one line.** `vmstat -v` prints the count *before* its label; the
   patterns were unanchored so `\s+(\d+)` matched across the newline. `node_jfs2_fsbuf_blocked_total`
   was reporting the **client** counter and `node_jfs2_client_fsbuf_blocked_total` the **external
   pager** counter. Now anchored `^…$`. **These two series change value on deploy.** Added
   `node_jfs2_external_pager_fsbuf_blocked_total` (the actual JFS2 counter; the plain one is JFS).
2. **`lsfs` diagnostics were parsed as filesystem rows.** `lsfs` prints `/dev/NAME: message` inline,
   which starts with `/dev/` exactly like a data row. Each stale device produced a phantom entry whose
   "VFS type" was the fourth word of the English error message; they were discarded only because those
   words happened not to equal `jfs2`. Fixed via `isLsfsTableRow` (device field must not end in `:`).
   Verified: the capture went from 34 parsed rows to the correct 27.
3. **`parseLsvgDashL`'s fallback used `parts[len-2]` as the state**, so a row without a `MOUNT POINT`
   column reported the **PVs count** as its state label. Now uses documented column order.
4. **perfstat `LogicalPartitions` was substituted for `lsvg -l`'s PPs column — this is wrong for
   mirrored LVs.** `LogicalPartitions` counts *logical* partitions; PPs counts *physical* ones. They
   diverge by the copy count, so a mirrored rootvg would have reported half its real
   `node_lv_pps_total`. perfstat is now a **fallback only**, used when the CLI row fails to parse.

   **This directly contradicts `findings.md` §3**, which concluded the substitution was safe based on an
   exact match across ~85 LVs. That evidence is real but insufficient: **every LV in the capture has
   `mirrors=1`**, so the fields could not have diverged. `Nimlv` has `PVs=3` with `mirrors=1` (spread,
   not mirrored), which makes this easy to miss. **`findings.md` §3 needs correcting** — see §12.

### 10.4 How to type-check for AIX off-AIX (this now works)

`CLAUDE.md` and §1 of this document previously said this was impossible. The blocker is only that
`perfstat`'s AIX functions are CGo-gated, so `CGO_ENABLED=0` leaves them undefined. Supply a pure-Go
stand-in via an alternate modfile plus an overlay for `cpu_aix.go` (the one collector file that uses
`import "C"` directly, and the only source of `tickPerSecond`):

```bash
# 1. stub module: copy perfstat's types_*.go + its !aix doc.go with the build tag stripped
# 2. alternate modfile:  go mod edit -modfile=/tmp/aixcheck.mod -replace github.com/power-devops/perfstat=<stub>
# 3. overlay cpu_aix.go with a pure-Go file providing tickPerSecond
GOFLAGS=-mod=mod GOOS=aix GOARCH=ppc64 CGO_ENABLED=0 \
  go build -modfile=/tmp/aixcheck.mod -overlay=/tmp/overlay.json ./collector/
```

Result: **clean**. This type-checks the real `fs_nfs_aix.go` against the real `aix` build tag. It does
not exercise CGo and does not replace the AIX build server.

Note `go vet` under `GOOS=aix` still fails on a **pre-existing, unrelated** issue: `os_release.go` is
tagged `//go:build !noosrelease && !aix`, but `os_release_test.go` carries no build tag, so the test
file references `osRelease` in a build where its definition is excluded. Not caused by this work —
`go vet ./collector/` on Linux passes cleanly.

### 10.5 Test harnesses (scratchpad, not committed)

Two standalone modules under the session scratchpad, both built by **extracting the functions verbatim**
from `fs_nfs_aix.go` rather than reimplementing them, so they cannot drift silently:

- `parsecheck/` — parser tests + `testdata/` fixtures cut verbatim from the real captures. Covers
  `parseLsfsQ`, `parseLsfsQAttrs`, `isLsfsTableRow`, `parseLsvgDashL`, `parseMountLogDevices`,
  `parseHdcryptmgrShowVG`, and the `lsvg`/`vmstat` regexes.
- `execcheck/` — exec-layer and concurrency tests under `-race`: WaitDelay leak reproduction and fix,
  goroutine-count regression, error classification, tolerant-vs-fail-closed runners, environment
  inheritance, `encryptionLookupPool` and `queryVolumeGroups` (exactly-once dispatch, bounded
  concurrency, clean partial results on deadline).

**If this work is carried forward, these should become real `_test.go` files in the repo.** They cannot
live in `collector/` as-is because the package is `//go:build aix` and cannot execute on Linux; the
practical options are a small non-tagged `collector/aixparse` package holding the pure parsers, or
`//go:build aix` tests that only run on the AIX build server.

### 10.6 Concurrent scrapes (two Prometheus HA replicas) — verified

**Concurrent `Update()` on a shared collector instance is real in this exporter, not theoretical:**

- `collector/collector.go` — `initiatedCollectors` (line ~54) caches collector instances **globally**;
  `NewNodeCollector` reuses them for every request. The same `*aixFSInfoCollector` pointer serves all
  scrapes.
- `node_exporter.go` — `promhttp.HandlerOpts{MaxRequestsInFlight: h.maxRequests}`, default **40**
  (`--web.max-requests`). Nothing serialises scrapes.
- `NodeCollector.Collect` spawns a goroutine per collector per `Gather`, so two overlapping HTTP scrapes
  produce two concurrent `Update()` calls on the same pointer.

**Shared-state audit of `fs_nfs_aix.go`** (`awk` over the struct definitions + `grep '^var [a-z]'`):

| Collector | Cross-scrape mutable state |
|---|---|
| `aix_vglv`, `aix_vmstat_fsbuf`, `aix_nfsstat` | **none** — descriptors + logger. Guard, context and result maps are per-`Update`. |
| `aix_fsinfo` | encryption cache (`encByDev`, `vgGate`) added by §10.2. Guarded by `encMu`. |
| package level | only `lvEncryptionRe` / `lsvgDashLRowRe` (compiled regexps are safe for concurrent use) and kingpin flag pointers (read-only after parse). |

**Tests** (`execcheck/concurrent_scrape_test.go`, all under `-race`). These fork **real** processes —
stub `hdcryptmgr` and `lslv` installed on `PATH` via `t.Setenv`, each appending to a log file — so fork
counts are measured, not assumed. Device set and LV→VG map are the real 19 mounted filesystems from the
nimserver capture.

| Scenario | Measured |
|---|---|
| 25 rounds × 2 concurrent replicas (50 scrapes) | 2 `hdcryptmgr`, 0 `lslv`; no races; every scrape returns a complete identical answer |
| 2 simultaneous cold scrapes, gate available | 2 `hdcryptmgr`, 0 `lslv` (serial: 1 and 0) |
| 2 simultaneous cold scrapes, no gate | **38 `lslv` vs 19** — the §6/§8.3 doubling, quantified |
| Gate, 1 of 3 VGs encrypted | 3 `lslv` instead of 19 |
| VG absent from gate output | falls through to `lslv`, does **not** default to "not encrypted" |
| Cache hit | does **not** refresh the entry timestamp (else TTL never expires on a busy host) |
| Device set shrinks | cache pruned to the current set, so it cannot grow unbounded |

**A note on writing these tests:** the first run "failed" three times because the fake `hdcryptmgr`
output listed 3 VGs while the LV→VG map referenced 5 — the two unlisted VGs correctly fell through to
`lslv`. The fixtures were wrong, not the code. That accident is now a deliberate test
(`TestGateUnknownVGFallsThroughToLslv`), because "gate does not mention this VG" is a real production
case: `hdcryptmgr showvg`'s header is `VG NAME / ID`, so a VG printed by ID would not match the
perfstat name map, and silently answering "no" for it would under-report encryption.

**Decision: no singleflight / in-flight deduplication was added.** Before this change set two replicas
cost 2 × N `lslv` forks *every scrape*; they now cost that at most once per 15-minute TTL, and on
AIX 7.2 TL5+/7.3 the gate reduces it to 2 `hdcryptmgr` forks. Dedup would save ~1 fork/minute in the
worst case while coupling the replicas' scrapes — a slow lookup for one would block the other. Revisit
only if a pre-TL5 host demonstrably exhausts its budget on this path.

**libperfstat thread-safety:** IBM documents libperfstat as a threadsafe 32/64-bit API, so the
concurrent `LogicalVolumeStat()` / `VolumeGroupStat()` calls two replicas produce are safe. IBM service
documentation separately records a known issue where `PERFSTAT_LOGICALVOLUME` reports failure when LVs
exist on a varied-off VG — this is precisely the `errno=6` in the nimserver capture (§11.1), and is
harmless because the Go wrapper checks only the return count.

---

## 11. Real-host validation — `nimserver` / `10.10.5.128` (AIX 7.3)

Source files: `commandoutput10105128.txt`, `coomandoutputlvvg.txt` (repo root). Used as **test
fixtures**, not read by eye.

### 11.1 What the captures confirmed

- `lsfs -q` (63 lines) → exactly **27 rows = 19 usable + 7 stale + 1 cdrfs**, matching the host.
- The 7 stale devices (`spot1`, `fslv02`, `infinettestlv`, `test1lv`, `test2lv`, `users1lv`, `userslv`)
  correspond exactly to the VGs perfstat reports varied off (`infivg`, `test1vg`, `test2vg`).
- **This closes §5's last open bullet** (the previously-unverified claim about `lsfs -q` behaviour on
  stale devices). Confirmed: it interleaves per-device errors, exits non-zero, and still prints complete
  data for every healthy filesystem. `runCmdWithGuardTolerant` was the right call.
- All 5 `lsvg -l` outputs (79 rows total) parse exactly, including `boot`/`paging`/`sysdump`/`jfs2log`/
  `jfslog`/`jfs` types.
- `VariedState == 0` = varied ON: the 5 VGs in `lsvg -o` are exactly the 5 of 9 with state 0. Third
  independent confirmation.
- `errno=6` on the perfstat fetch call is **harmless** — the Go wrapper checks only `r < 0`
  (`lvmstat.go`), so `VolumeGroupStat()` returns all 9 VGs. The CLI fallback is not being hit.
- LVs in varied-off VGs return with an **empty VG name** (`infiloglv`, `infinettestlv`, `state=0`). The
  encryption VG gate correctly treats `""` as unknown and falls through to `lslv`.
- Timings on a healthy host: `lsvg -o` 0.00s, `lsvg -l` 0.01-0.03s, `lsfs -q` 0.06s, `lssrc -a` 0.01s.

### 11.2 The leak baseline — a negative control, not a refutation

`ls -la /proc/12517786/fd | wc -l` = **9**; `ps -o thcount` = **18**; uptime **6d 04:34:54**; no child
processes, no defunct processes. That is a flat, healthy baseline — and it is **consistent with** §10.1
rather than contrary to it: the leak only triggers when a command is *killed mid-flight*, and on this
host nothing ever reaches the deadline (see timings above). The trend that matters is still the one
`rca.md` §8 asked for on `.81`/`.82`, where timeouts actually fire.

Also visible in the capture: the deployed binary runs `aix_srcstatus`, `aix_lspath`, `aix_process`,
`aix_timebase`, `aix_diskqueues`, `aix_hypervisor`, `aix_netadapter`, `aix_scheduler` — **none of whose
source files exist in this tree** (§1). They fork subprocesses and still carry the §10.1 leak.

---

## 12. Open items after this session

1. **`filesystem` collector still has no timeout protection** (24.2s observed). Now the largest
   remaining contributor. Cannot be fixed with a context — `perfstat.FileSystemStat()` is a CGo call and
   is not cancellable; needs caching / stale-while-refresh or a watchdog.
2. **Apply the `WaitDelay` fix to the other AIX collector files** once they are available in a tree
   (`srcstatus_aix.go`, `lspath_aix.go`, `process_aix.go`, …). Preferably by routing them through the
   shared `runCommandRaw` rather than duplicating it.
3. **Correct `findings.md` §3** per §10.3 defect #4. To settle it properly, capture `lsvg -l` plus the
   perfstat dump from a host with a **mirrored** LV (`PPs = 2 × LPs`) and check whether
   `PPs = LogicalPartitions × Mirrors` holds. If it does, perfstat can be promoted back to primary with
   the multiplication applied.
4. **Verify `lslv` actually prints `ENCRYPTION:` on AIX 7.3.** IBM's documented interface is
   `hdcryptmgr showlv`, and the published `lslv` field list does not include it. If it does not,
   `node_filesystem_encryption_enabled` has been 0 everywhere regardless of the truth. Check:
   `lslv hd4 | grep -i encrypt`. If absent, switch layer 3 of the encryption chain to `hdcryptmgr
   showlv`; the VG gate is unaffected.
5. **Confirm `hdcryptmgr` is present fleet-wide** (needs AIX 7.2 TL5+). If absent the code falls back
   silently and correctly — only the fork saving is lost.
6. **Dashboard/alert check before deploy** for `node_jfs2_fsbuf_blocked_total` and
   `node_jfs2_client_fsbuf_blocked_total` (§10.3 defect #1) — both change value.
7. **`diagrams.md` is stale** and still describes the §4 code state.
8. Promote the §10.5 harnesses into committed tests.