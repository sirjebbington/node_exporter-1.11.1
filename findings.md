# Findings: aix_vglv perfstat migration — evidence and scope

## Objective

Reduce subprocess load in the `aix_vglv` collector (`node_exporter/collector/fs_nfs_aix.go`)
by replacing the VG-listing step (`lsvg -o`) and the LV physical-partition count
(currently read from `lsvg -l`'s PPs column) with direct libperfstat CGo calls
(`perfstat.VolumeGroupStat()`, `perfstat.LogicalVolumeStat()`), which make zero
subprocess forks. This is a partial migration — see "Out of scope" below for the
fields staying on CLI parsing, and why.

This follows two earlier fixes to the same file:
1. Removed the redundant per-LV `lslv` loop in `aix_vglv` (PPs/state parsed
   directly from `lsvg -l`'s output instead of re-fetching per LV).
2. Removed the redundant per-filesystem `lsfs -q` loop in `aix_fsinfo` (one
   whole-system call instead of one call per mountpoint), plus a fix for AIX's
   `lsfs -q` non-zero exit on stale devices, plus a new `node_filesystem_stale_info`
   metric to surface orphaned LV/filesystem config instead of silently dropping it.

## Evidence

All findings below are backed by two independent C-program runs against real
`libperfstat` on host `10.10.5.128` (nimserver), captured in
`Nodeexporter_logs/10.10.5.128/coomandoutputlvvg.txt`. The test programs called
`perfstat_volumegroup()` and `perfstat_logicalvolume()` directly via CGo and
printed every relevant field, cross-referenced against `lsvg -o`, `lsvg -l Nimvg`,
and `lsvg -l rootvg` output captured in the same session.

### 1. `VolumeGroup.VariedState` — confirmed mapping, IBM-documented and empirically verified

IBM's official documentation (`libperfstat.h` file reference, AIX 7.3.0), extracted
from a locally saved copy of the doc page:

> `unsigned variedState` — Volume Group is Available or not
> `0` = Available, which implies varied ON
> `1` = Not Available, which implies varied OFF

This is the **inverse** of what would normally be assumed (`1 = on` is the more
common convention) — the IBM documentation was essential here; guessing would
very likely have produced the wrong mapping and silently corrupted this field
across the whole fleet.

Cross-checked against real AIX output, 9 VGs on nimserver:

| VG | `variedState` | In `lsvg -o`? | Consistent with docs? |
|---|---|---|---|
| Nimvg | 0 | yes | yes (0=ON) |
| rootvg | 0 | yes | yes (0=ON) |
| testvg | 0 | yes | yes (0=ON) |
| test_newvg | 0 | yes | yes (0=ON) |
| testvg_new1 | 0 | yes | yes (0=ON) |
| altinst_rootvg | 1 | no | yes (1=OFF) |
| infivg | 1 | no | yes (1=OFF) |
| test1vg | 1 | no | yes (1=OFF) |
| test2vg | 1 | no | yes (1=OFF) |

Zero exceptions across 9 VGs, confirmed on two independent test runs (the second
run is the one captured in `coomandoutputlvvg.txt`). **Verdict: safe to use.**

### 2. `VolumeGroup.TotalLogicalVolumes` / `OpenedLogicalVolumes` — exact match, not currently needed but confirmed reliable

Cross-checked against `lsvg -l <vg>` LV counts:

- **Nimvg**: perfstat reports `totalLVs=55, openedLVs=4`. Counting rows in
  `lsvg -l Nimvg`: 55 total LV rows, exactly 4 marked `open/syncd`
  (`Nimloglv`, `Nimlv`, `fslv00`, `spotlv`), the remaining 51 `closed/syncd`.
  **Exact match.**
- **rootvg**: perfstat reports `totalLVs=18, openedLVs=17`. Counting rows in
  `lsvg -l rootvg`: 18 total, only `hd5` is `closed/syncd`, the other 17
  `open/syncd`. **Exact match.**

Not part of this migration's scope (no current metric uses VG-level LV counts),
but confirms the VolumeGroup struct's other fields are trustworthy, which is
useful context for any future work.

### 3. `LogicalVolume.LogicalPartitions` — exact match against `lsvg -l`'s PPs/LPs column

Checked across ~15 LVs spanning both `Nimvg` and `rootvg`:

| LV | perfstat `LPs` | `lsvg -l` PPs column | Match? |
|---|---|---|---|
| Nimloglv | 1 | 1 | yes |
| Nimlv | 500 | 500 | yes |
| fslv00 | 200 | 200 | yes |
| spotlv | 320 | 320 | yes |
| hd5 | 1 | 1 | yes |
| hd6 | 68 | 68 | yes |
| hd8 | 1 | 1 | yes |
| hd4 | 20 | 20 | yes |
| fslv11 | 6 | 6 | yes |
| fslv09 | 9 | 9 | yes |
| (and ~5 more, all matching) | | | yes |

Every value cross-checked matches exactly, zero deviations across ~85 LVs total
in the raw dump. **Verdict: safe to use as a direct replacement for the PPs
column currently parsed from `lsvg -l` for the `node_lv_pps_total` metric.**

### 4. `LogicalVolume.OpenClose` — partially decodable, NOT sufficient to migrate `node_lv_state`

Real data shows:

- `open_close=1` corresponds to every LV shown as `open/...` in `lsvg -l`.
- `open_close=2` corresponds to every LV shown as `closed/...` in `lsvg -l`.

This is a clean, consistent split across ~85 LVs with zero exceptions — this
half of the state string (open vs. closed) is safely decodable.

**However**, the second half of the state string (`syncd` vs. `stale`, e.g.
`open/syncd` vs. a hypothetical `open/stale`) is carried by the separate
`LogicalVolume.State` field (raw `LVM_*` constant, e.g. `LVM_UNDEF` — see
`lvm.h`), and **every single LV in the real dataset shows `state=1`**,
regardless of whether it's open or closed. There is no example of a stale/
non-syncd LV anywhere in the captured data, so there is no way to empirically
confirm what `state=1` actually represents, or what value a stale LV would show.

Per the original task's explicit instruction — do not guess this mapping, an
incorrect guess would silently corrupt `node_lv_state` fleet-wide — **this field
stays on CLI parsing** (`lsvg -l`'s `LV STATE` column, as today). Migrating only
the open/closed half while guessing the sync half is not an acceptable partial
step; the metric's value is the combined string, and half-guessed data is worse
than fully CLI-sourced data.

### 5. Fields with no perfstat equivalent at all — confirmed by struct inspection, unchanged from original scope

Re-confirmed by reading the actual vendored struct definitions
(`vendor/github.com/power-devops/perfstat/types_lvm.go`):

- `VolumeGroup` has no free/total PPs field. `node_vg_free_pps_total` /
  `node_vg_total_pps` must keep calling `lsvg <vg>` and parsing `FREE PPs:` /
  `TOTAL PPs:`.
- `LogicalVolume` has no mountpoint field. LV-to-mountpoint association for
  `node_lv_pps_total{lv,mountpoint}` and `node_lv_state{mountpoint,state}` must
  keep coming from `lsvg -l`'s MOUNT POINT column.
- Encryption (the `lslv`-derived `ENCRYPTION:` field used by `aix_fsinfo`, a
  separate collector) has no perfstat equivalent. Out of scope for this task
  regardless.

### 6. `EnableLVMStat()` — required prerequisite, confirmed present in the vendored package

IBM's documentation states LVM statistics collection is disabled by default and
must be enabled via `perfstat_config(PERFSTAT_ENABLE | PERFSTAT_LV, NULL)`. The
vendored package already exposes this as `perfstat.EnableLVMStat()`
(`vendor/github.com/power-devops/perfstat/config.go:13-15`). Neither
`LogicalVolumeStat()` nor `VolumeGroupStat()` call it internally — it is the
caller's responsibility. This must be called once (not per-scrape) before either
function is used; the current test runs called it once at program start and both
calls succeeded, confirming this is sufficient.

Also confirmed: an initial test (on a different host, before `EnableLVMStat`
semantics were understood) saw `perfstat_volumegroup()` return a `Permission
denied` (`errno=13`) error when run without root privilege — this was resolved
by running as root, matching node_exporter's actual runtime privilege (confirmed
in production logs: `"Node Exporter is running as root user"`). This is not
expected to be a blocker in production but is a real runtime dependency worth
being aware of if the collector is ever run as a
non-root service account.

## Scope decision — final

| Metric | Data source after this change | Reason |
|---|---|---|
| VG listing (which VGs to iterate) | `perfstat.VolumeGroupStat()`, filtered `VariedState == 0` | Confirmed correct mapping, zero subprocess forks |
| `node_lv_pps_total` (PPs value) | `perfstat.LogicalVolumeStat()`'s `LogicalPartitions`, joined by VG name + LV name | Exact match confirmed against real data |
| `node_lv_pps_total` (mountpoint label) | unchanged — `lsvg -l <vg>` | No perfstat equivalent |
| `node_vg_free_pps_total` / `node_vg_total_pps` | unchanged — `lsvg <vg>` | No perfstat equivalent |
| `node_lv_state` | unchanged — `lsvg -l <vg>`'s LV STATE column | State-half of the string not empirically verifiable; do not guess on a prod fleet metric |
| `aix_fsinfo` (all fields) | unchanged | Out of scope; no perfstat equivalent for lsfs -q data at all |
| Encryption (`lslv`) | unchanged | No perfstat equivalent |

## Expected improvement

Today, `aix_vglv.Update()` per scrape cycle issues:
- 1 `lsvg -o` call (VG listing)
- 1 `lsvg <vg>` call per VG (free/total PPs)
- 1 `lsvg -l <vg>` call per VG (LV rows: name, PPs, state, mountpoint)

Total: `1 + 2N` subprocess forks, where N = number of varied-on VGs.

After this change:
- 0 forks for VG listing (replaced by one CGo call, `perfstat.VolumeGroupStat()`)
- 1 `lsvg <vg>` call per VG (free/total PPs — unchanged, no perfstat equivalent)
- 1 `lsvg -l <vg>` call per VG (state + mountpoint — unchanged; PPs value itself
  will be cross-populated from perfstat instead of parsed from this same output,
  but the call must still happen for state/mountpoint)

Total: `2N` subprocess forks, down from `1 + 2N`.

This is a smaller reduction than the earlier `lslv`-loop fix (which removed a
per-LV `1:N` fork multiplier), because most of `aix_vglv`'s remaining subprocess
calls have no perfstat equivalent and must stay. The concrete benefit here is:
- One fewer fork per scrape (`lsvg -o` eliminated), and that one fork is no
  longer subject to `TimeoutGuard`'s budget or `context.DeadlineExceeded` risk
  at all, since it's a direct library call with no subprocess/timeout involved.
- `node_lv_pps_total`'s PPs value becomes sourced from a call with zero
  timeout/parsing risk, though it is cross-validated against the still-present
  `lsvg -l` parse (see implementation plan) rather than blindly trusted, since
  `lsvg -l` must still run for state/mountpoint regardless.

This is a modest, low-risk optimization — not a repeat of the earlier
high-impact loop fix. It is being done because it is safe and verified, not
because it is expected to meaningfully move the timeout-budget needle on its
own.