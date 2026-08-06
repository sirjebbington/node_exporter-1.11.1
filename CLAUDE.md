# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this repo is

A customized fork of Prometheus Node Exporter targeting **IBM AIX ppc64** systems. The `node_exporter/` subdirectory contains the Go source; all other directories are infrastructure (build scripts, CI pipeline, deploy scripts).

## Build

Building requires an AIX server (or AIX cross-compiler). It cannot be built natively on macOS/Linux without a cross-toolchain.

**On an AIX build server:**
```bash
cd node_exporter
export GOOS=aix GOARCH=ppc64 CGO_ENABLED=1
export CC=/opt/freeware/bin/gcc-13
export PATH="/home/obsmonuat/build/go1.25.0/go/bin:$PATH"
export GOPROXY=direct GOSUMDB=off  # offline build, uses vendor/
go build -mod=vendor -o node_exporter .
```

The build is offline-only — `vendor/` is committed and must be present; no module proxy access is expected.

**Verify the binary:**
```bash
./node_exporter --version
./node_exporter --help | grep web.listen-address
```

## Tests

```bash
cd node_exporter
go test ./...               # all tests
go test ./collector         # collector package only
go test -v ./...            # verbose
```

Tests run on any platform (Linux/macOS) even though the binary targets AIX, because non-AIX collectors are excluded via build tags.

## CI/CD pipeline

**File:** `pipelines/azure-build-nodeexporter-aix.yml`

Three active stages:
1. **source_preparation** — runs on `CI_Shared_Linux`; validates source, runs Semgrep SAST, archives `node_exporter/` as `node-exporter-source.zip`, outputs build metadata variables.
2. **aix_build** — runs on `UAT` pool (AIX agent); downloads source zip, injects credentials/version via `replacetokens@5` into `build-scripts/build-aix-node-exporter.ps1`, then executes it; the PowerShell script SSHes into the AIX build server (`10.70.20.186` by default) via PuTTY and runs `build-scripts/build-node-exporter-aix.sh` remotely; the resulting `node_exporter-<version>.aix-ppc64.tar.gz` is published as a pipeline artifact.
3. **publish_artifacts** — runs on `CI_Shared_Linux`; **only runs when triggered from a git tag** (`refs/tags/*`); publishes the tar.gz to the `Observability` Universal Package feed.

Stage 4 (deployment) is commented out in the YAML.

**To publish a release:**
```bash
git tag v1.11.1
git push origin v1.11.1
```

Token replacement in `build-aix-node-exporter.ps1` uses `#{token}#` syntax (replaced by `replacetokens@5`). Credentials come from the `builder-tools` variable group (ARCOS UAT).

## Collector architecture

All collectors live in `node_exporter/collector/`. The framework is in `collector.go`; collectors self-register via `init()` calling `registerCollector(name, defaultEnabled, factory)`.

**All AIX-specific collectors are disabled by default** (`defaultDisabled`) and must be explicitly enabled with `--collector.<name>`.

File naming convention:
- `*_aix.go` — AIX-only, guarded by `//go:build aix`
- `*_common.go` / `*_linux.go` / `*_darwin.go` etc. — platform-specific variants
- No build tag = cross-platform

AIX-specific collectors (16 files, 11 registered collectors):

| File | Collector name | Data source |
|------|---------------|-------------|
| `cpu_aix.go` | `cpu` | `perfstat` CGo library |
| `meminfo_aix.go` | `meminfo` | `svmon` command |
| `diskstats_aix.go` | `diskstats` | `iostat -d` |
| `netdev_aix.go` | `netdev` | `netstat` |
| `filesystem_aix.go` | `filesystem` | JFS2-aware mount parsing |
| `lspath_aix.go` | `aix_lspath` | `lspath` (SAN/FC paths) |
| `srcstatus_aix.go` | `aix_srcstatus` | `lssrc -a` (SRC services) |
| `hypervisor_aix.go` | `aix_hypervisor` | `lparstat` (LPAR config) |
| `diskqueues_aix.go` | `aix_diskqueues` | disk I/O queue stats |
| `netadapter_aix.go` | `aix_netadapter` | network adapter details |
| `scheduler_aix.go` | `aix_scheduler` | process scheduler |
| `timebase_aix.go` | `aix_timebase` | POWER CPU timebase |
| `fs_nfs_aix.go` | `aix_nfsstat` | NFS statistics |
| `lspath_aix.go` / `hypervisor_aix.go` / others | `aix_fsinfo`, `aix_vglv`, `aix_vmstat_fsbuf` | `lsvg`/`lslv`, vmstat |

CGo is required for `cpu_aix.go` (uses `github.com/power-devops/perfstat`). Most other AIX collectors use `exec.CommandContext` with timeouts.

## Adding a new AIX collector

1. Create `collector/aix_<feature>.go` with `//go:build aix` tag.
2. Define a struct implementing `Update(ch chan<- prometheus.Metric) error`.
3. Register in `init()`: `registerCollector("aix_<feature>", defaultDisabled, NewMyCollector)`.
4. Use `exec.CommandContext` with a timeout for any shell commands; return `nil` on non-fatal errors so the scrape continues.

## Key version info

- Node Exporter version: **1.11.1**
- Go: **1.25.0+** at `/home/obsmonuat/build/go1.25.0/go/bin/go` on AIX
- GCC: **13.x** at `/opt/freeware/bin/gcc-13` on AIX
- Target: AIX 7.1+ ppc64
- Metrics endpoint: `:9100/metrics`
- Service managed via AIX SRC: `startsrc/stopsrc -s node_exporter_aix_go`
