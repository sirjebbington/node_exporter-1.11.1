// collector/fs_nfs_aix.go
//go:build aix
// +build aix

package collector

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/power-devops/perfstat"
	"github.com/prometheus/client_golang/prometheus"
)

// ----------------------------------------------------------------------------
// Flags
// ----------------------------------------------------------------------------

var (
	// AIX LVM commands take an ODM lock on the volume group and, by default,
	// retry until they get it ("Volume group is locked. This command will
	// continue retries until lock is free."). A concurrent chfs/extendlv/
	// mirrorvg, or a SAN event that stalls one, therefore parks lsvg/lslv for
	// as long as the lock is held — the exact mechanism behind the "per-call
	// latency creeps toward 1-2s under load" behaviour in rca.md §3.
	//
	// -L skips the lock entirely. IBM's caveat is that data may be
	// inconsistent *while a VG is actively being modified*; for a monitoring
	// exporter, momentarily stale partition counts are strictly better than a
	// stalled scrape that drops every other collector's metrics with it
	// (rca.md §4).
	aixLVMNoLock = kingpin.Flag(
		"collector.aix_vglv.no-lock",
		"Pass -L to lsvg/lslv so LVM queries never block waiting for a volume group ODM lock. Values can be momentarily stale while a VG is being modified.",
	).Default("true").Bool()

	// Volume groups are queried independently of one another, so the per-VG
	// lsvg calls are fanned out instead of run back to back. Wall-clock cost
	// drops from 2N serial forks to roughly 2N/concurrency.
	aixVGLVConcurrency = kingpin.Flag(
		"collector.aix_vglv.concurrency",
		"Number of volume groups queried in parallel by the aix_vglv collector.",
	).Default("4").Int()

	// Logical volume encryption is a provisioning-time property; re-reading it
	// on every scrape is pure waste. See encryptionStatus for the full
	// fallback chain.
	aixFSInfoEncryptionTTL = kingpin.Flag(
		"collector.aix_fsinfo.encryption-cache-ttl",
		"How long logical volume encryption status is cached by the aix_fsinfo collector. 0 disables caching.",
	).Default("15m").Duration()
)

// ----------------------------------------------------------------------------
// Command execution
//
// Every AIX command issued by this file goes through runCommandRaw so that the
// WaitDelay guard, the C locale and the deadline classification below are
// applied uniformly.
// ----------------------------------------------------------------------------

// cmdWaitDelay bounds how long exec.Cmd.Wait may block on the command's I/O
// pipes once the process itself has exited or been killed.
//
// This is the fix for the goroutine/descriptor leak in rca.md §8.2. With
// WaitDelay left at its zero value, os/exec reads the stdout/stderr pipe until
// EOF, and (quoting os/exec) EOF "might not occur until orphaned subprocesses
// of the command have also closed their descriptors for the pipes". AIX LVM
// and filesystem commands are ksh wrappers that fork helpers (lqueryvg,
// getlvodm, ...) which inherit that pipe. Killing the wrapper when the context
// deadline fires does not close the inherited write end, so io.Copy blocks
// forever, Cmd.Wait never returns, and the calling goroutine plus one pipe
// pair leak permanently — one per timed-out command, cleared only by
// restarting the process. That is exactly the day-over-day escalation with
// full recovery on restart observed on .81/.82/custdbdev.
//
// With WaitDelay set, Wait force-closes the parent's pipe ends after the
// delay, the copy goroutine unblocks, and Wait always returns.
const cmdWaitDelay = 2 * time.Second

// newAIXCommand builds a command bounded by ctx with the leak guard applied.
func newAIXCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	// Inherit the process environment — the ksh wrappers around the AIX LVM
	// and filesystem commands need PATH/ODMDIR/LIBPATH — but force the C
	// locale last so column headers and number formatting stay parseable.
	// (os/exec keeps the last occurrence of a duplicated variable.)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	cmd.WaitDelay = cmdWaitDelay
	return cmd
}

// runCommandRaw runs name bounded by ctx and returns its combined
// stdout+stderr. Output is returned even alongside an error, because several
// AIX commands exit non-zero over a single bad device while still printing
// complete, correct data for every other one.
//
// A context deadline or cancellation is reported as an error wrapping ctx.Err()
// so callers can classify it with errors.Is, regardless of whether the kill
// surfaced as an ExitError, as ErrWaitDelay, or as the context error itself.
func runCommandRaw(ctx context.Context, name string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s %v not started, budget exhausted: %w", name, args, err)
	}
	out, err := newAIXCommand(ctx, name, args...).CombinedOutput()
	if err == nil {
		return out, nil
	}
	if cerr := ctx.Err(); cerr != nil {
		return out, fmt.Errorf("%s %v timed out: %w", name, args, cerr)
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		// The command completed on its own but left a helper process holding
		// the pipe open. The output we have is what it printed, and the leak
		// is already contained by WaitDelay, so this is not a failure.
		return out, nil
	}
	return out, fmt.Errorf("%s %v failed: %w (out=%s)", name, args, err, bytes.TrimSpace(out))
}

// runCommand fails closed: a non-zero exit discards the output.
func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := runCommandRaw(ctx, name, args...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// runCommandTolerant keeps whatever the command printed alongside a non-zero
// exit. This matters for `lsfs -q` with no filesystem argument: AIX exits
// non-zero as soon as ANY listed device is stale (a deleted or varied-off LV
// still referenced in /etc/filesystems), even though it prints complete,
// correct data for every other filesystem. Discarding that output over one bad
// device would drop every filesystem's metrics, not just the stale one.
func runCommandTolerant(ctx context.Context, name string, args ...string) ([]byte, error) {
	return runCommandRaw(ctx, name, args...)
}

// isDeadline reports whether err was caused by the collector running out of
// time budget rather than by the command itself failing.
func isDeadline(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// lsvgArgs / lslvArgs prefix -L when no-lock mode is enabled. -L is a leading
// flag in both commands' documented syntax:
//
//	lsvg [ -L ] [ -o ] | [ -n PV ] | [ -i ] [ -l | -M | -p ] VolumeGroup ...
//	lslv [ -L ] [ -l | -m ] [ -n PV ] LogicalVolume
func lsvgArgs(args ...string) []string {
	if *aixLVMNoLock {
		return append([]string{"-L"}, args...)
	}
	return args
}

func lslvArgs(args ...string) []string {
	if *aixLVMNoLock {
		return append([]string{"-L"}, args...)
	}
	return args
}

// ----------------------------------------------------------------------------
// Shared parsing helpers
// ----------------------------------------------------------------------------

// newScanner returns a bufio.Scanner with an enlarged buffer to tolerate
// wide AIX outputs and column drift.
func newScanner(b []byte) *bufio.Scanner {
	sc := bufio.NewScanner(bytes.NewReader(b))
	buf := make([]byte, 0, 256*1024) // 256 KiB initial
	sc.Buffer(buf, 1024*1024)        // allow up to 1 MiB lines
	return sc
}

// isUnmounted normalizes common textual representations of "no mountpoint".
func isUnmounted(mp string) bool {
	m := strings.ToUpper(strings.TrimSpace(mp))
	switch m {
	case "", "N/A", "--", "NO", "NO MOUNT POINT", "UNMOUNTED":
		return true
	default:
		return false
	}
}

// grepInt returns the first capture group of re parsed as an integer, or 0.
// Callers pass a package-level compiled regexp: these run once per volume
// group per scrape, so compiling on every call was measurable waste.
func grepInt(re *regexp.Regexp, out []byte) int64 {
	if m := re.FindSubmatch(out); len(m) >= 2 {
		v, _ := strconv.ParseInt(string(m[1]), 10, 64)
		return v
	}
	return 0
}

// firstInt parses the leading integer of a whitespace-separated value such as
// "4096" or "270 (138240 megabytes)".
func firstInt(s string) (int64, bool) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0, false
	}
	v, err := strconv.ParseInt(f[0], 10, 64)
	return v, err == nil
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// ----------------------------------------------------------------------------
// Filesystem Info (mount, lsfs -q, hdcryptmgr, lslv)
// Enable with: --collector.aix_fsinfo
// ----------------------------------------------------------------------------

// fsEntry is one row of `lsfs -q` output. attrs is nil when lsfs printed the
// row but could not read the filesystem's superblock — a stale device.
type fsEntry struct {
	dev   string
	mp    string
	vfs   string
	attrs *fsAttrs
}

type fsAttrs struct {
	blockSize int64
	inlineLog bool
	sparseYes bool
	quotaYes  bool
}

func init() {
	registerCollector("aix_fsinfo", defaultDisabled, NewAIXFSInfoCollector)
}

type aixFSInfoCollector struct {
	blkSizeDesc   *prometheus.Desc // node_filesystem_block_size_bytes{mountpoint}
	inlineLogDesc *prometheus.Desc // node_filesystem_inline_log_enabled{mountpoint}
	logInfoDesc   *prometheus.Desc // node_filesystem_log_device_info{mountpoint,logdev}
	encryptDesc   *prometheus.Desc // node_filesystem_encryption_enabled{mountpoint}
	sparseDesc    *prometheus.Desc // node_filesystem_sparse_files_enabled{mountpoint}
	quotaDesc     *prometheus.Desc // node_filesystem_quota_enabled{mountpoint}
	staleDesc     *prometheus.Desc // node_filesystem_stale_info{mountpoint,device}
	logger        *slog.Logger

	// Encryption lookup state. Guarded by encMu because two Prometheus HA
	// replicas scrape every host on the same schedule (rca.md §8.3), so two
	// Update calls can overlap.
	encMu    sync.Mutex
	encByDev map[string]encEntry
	vgGate   *vgCryptoGate
}

type encEntry struct {
	enabled bool
	at      time.Time
}

// vgCryptoGate caches `hdcryptmgr showvg`. byVG is nil when hdcryptmgr is
// unavailable or its output was unparseable, which is remembered so a fleet
// without the command does not re-fork it on every scrape.
type vgCryptoGate struct {
	byVG map[string]bool
	at   time.Time
}

func NewAIXFSInfoCollector(logger *slog.Logger) (Collector, error) {
	if logger == nil {
		logger = slog.Default()
	}
	ns := namespace
	return &aixFSInfoCollector{
		blkSizeDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "filesystem", "block_size_bytes"),
			"Filesystem block size in bytes (from lsfs -q).",
			[]string{"mountpoint"}, nil,
		),
		inlineLogDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "filesystem", "inline_log_enabled"),
			"Whether the filesystem uses inline journal (1) or external log device (0).",
			[]string{"mountpoint"}, nil,
		),
		logInfoDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "filesystem", "log_device_info"),
			"Filesystem log device info (INLINE or /dev/hd8, etc.).",
			[]string{"mountpoint", "logdev"}, nil,
		),
		encryptDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "filesystem", "encryption_enabled"),
			"Whether the backing logical volume has encryption enabled (1=yes, 0=no).",
			[]string{"mountpoint"}, nil,
		),
		sparseDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "filesystem", "sparse_files_enabled"),
			"Whether sparse files are enabled (1=yes, 0=no).",
			[]string{"mountpoint"}, nil,
		),
		quotaDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "filesystem", "quota_enabled"),
			"Whether quota is enabled (1=yes, 0=no).",
			[]string{"mountpoint"}, nil,
		),
		staleDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "filesystem", "stale_info"),
			"Set to 1 for a filesystem listed in /etc/filesystems whose backing device lsfs -q could not read (e.g. a deleted or varied-off LV). No other node_filesystem_* metrics are emitted for this mountpoint.",
			[]string{"mountpoint", "device"}, nil,
		),
		logger: logger,
	}, nil
}

func (c *aixFSInfoCollector) Update(ch chan<- prometheus.Metric) error {
	guard := NewTimeoutGuard("aix_fsinfo", c.logger, 0)
	ctx, cancel := guard.Context()
	defer cancel()

	// 1) Mount table, for the per-mountpoint log device.
	mountOut, err := runCommand(ctx, "mount")
	if err != nil {
		if isDeadline(err) {
			guard.FlagTimeout("mount_timeout")
			guard.EmitIfTimedOut(ch)
		}
		return err
	}
	logMap := parseMountLogDevices(mountOut)

	// 2) One `lsfs -q` covers every filesystem on the host AND carries the
	// same base table that plain `lsfs` prints — IBM documents -q as showing
	// superblock detail "in addition to other file system characteristics
	// reported by the lsfs command". The separate plain `lsfs` call this
	// collector used to make was therefore duplicate work and has been
	// removed, taking the collector's fixed cost from 3 forks to 2.
	//
	// The tolerant runner is required here: AIX exits non-zero as soon as any
	// single device is stale but still prints valid data for every other
	// filesystem.
	qOut, err := runCommandTolerant(ctx, "lsfs", "-q")
	if err != nil {
		if isDeadline(err) {
			guard.FlagTimeout("lsfs_q_timeout")
			guard.EmitIfTimedOut(ch)
			return err
		}
		if len(qOut) == 0 {
			return err
		}
		c.logger.Debug("lsfs -q reported errors for one or more devices; parsing remaining output", "err", err)
	}

	var usable, stale []fsEntry
	for _, fs := range parseLsfsQ(qOut) {
		// Only JFS/JFS2 filesystems. Filtering on VFS type rather than on a
		// "--" size keeps stale jfs2 entries (whose size reads as "--"
		// exactly like an empty cdrfs CD-ROM drive) in scope so they can be
		// flagged below.
		if fs.vfs != "jfs2" && fs.vfs != "jfs" {
			continue
		}
		if isUnmounted(fs.mp) {
			continue
		}
		if fs.attrs == nil {
			stale = append(stale, fs)
			continue
		}
		usable = append(usable, fs)
	}

	// Surface orphaned LV/filesystem config instead of dropping it silently.
	for _, fs := range stale {
		ch <- prometheus.MustNewConstMetric(c.staleDesc, prometheus.GaugeValue, 1, fs.mp, fs.dev)
	}

	devs := make([]string, 0, len(usable))
	for _, fs := range usable {
		devs = append(devs, fs.dev)
	}
	encByDev := c.encryptionStatus(ctx, devs)

	for _, fs := range usable {
		mp := fs.mp
		ch <- prometheus.MustNewConstMetric(c.blkSizeDesc, prometheus.GaugeValue, float64(fs.attrs.blockSize), mp)
		ch <- prometheus.MustNewConstMetric(c.inlineLogDesc, prometheus.GaugeValue, boolToFloat(fs.attrs.inlineLog), mp)
		logdev := logMap[mp]
		if logdev == "" {
			logdev = "UNKNOWN"
		}
		ch <- prometheus.MustNewConstMetric(c.logInfoDesc, prometheus.GaugeValue, 1, mp, logdev)
		ch <- prometheus.MustNewConstMetric(c.sparseDesc, prometheus.GaugeValue, boolToFloat(fs.attrs.sparseYes), mp)
		ch <- prometheus.MustNewConstMetric(c.quotaDesc, prometheus.GaugeValue, boolToFloat(fs.attrs.quotaYes), mp)
		ch <- prometheus.MustNewConstMetric(c.encryptDesc, prometheus.GaugeValue, boolToFloat(encByDev[fs.dev]), mp)
	}
	guard.EmitIfTimedOut(ch)
	return nil
}

// parseLsfsQ parses `lsfs -q` output. Each filesystem is one table row
// starting with its /dev/ device, optionally followed by an indented,
// parenthesised attribute line carrying the superblock detail:
//
//	Name         Nodename  Mount Pt  VFS   Size    Options  Auto Accounting
//	/dev/hd4     --        /         jfs2  786432  --       yes  no
//	  (lv size: 786432, fs size: 786432, block size: 4096, sparse files: yes, ...)
//
// A row with no attribute line is a device whose superblock lsfs could not
// read — typically a deleted or varied-off LV still listed in
// /etc/filesystems. It is returned with attrs == nil rather than dropped, so
// the caller can flag it instead of silently losing the mountpoint.
//
// lsfs reports those failures inline, and its diagnostics begin with the
// device name too:
//
//	/dev/spot1      --         /spot1     jfs2  --   --   yes  no
//	/dev/spot1: A file or directory in the path name does not exist.
//	The volume group of /dev/spot1  may be varied off.
//
// The first of those two lines is indistinguishable from a table row by its
// /dev/ prefix alone, so isLsfsTableRow additionally requires that the device
// field not end in a colon. Without that check each stale device produces a
// phantom entry whose "VFS type" is whatever the fourth word of the English
// error message happens to be.
func parseLsfsQ(out []byte) []fsEntry {
	var res []fsEntry
	var cur fsEntry
	flush := func() {
		if cur.dev != "" {
			res = append(res, cur)
		}
	}
	sc := newScanner(out)
	for sc.Scan() {
		line := sc.Text()
		if parts, ok := isLsfsTableRow(line); ok {
			flush()
			// Name Nodename "Mount Pt" VFS Size Options Auto Accounting
			cur = fsEntry{dev: parts[0], mp: parts[2], vfs: parts[3]}
			continue
		}
		if cur.dev == "" {
			continue
		}
		t := strings.TrimLeft(line, " \t")
		if !strings.HasPrefix(t, "(") {
			continue
		}
		if a := parseLsfsQAttrs(t); a != nil {
			cur.attrs = a
		}
	}
	flush()
	return res
}

// isLsfsTableRow reports whether line is a filesystem row of `lsfs` output
// rather than one of the diagnostics lsfs interleaves for unreadable devices.
func isLsfsTableRow(line string) ([]string, bool) {
	if !strings.HasPrefix(line, "/dev/") {
		return nil, false
	}
	parts := strings.Fields(line)
	// A table row always carries at least Name/Nodename/Mount Pt/VFS, and its
	// device field is a bare path; "/dev/spot1:" is the prefix of an error.
	if len(parts) < 4 || strings.HasSuffix(parts[0], ":") {
		return nil, false
	}
	return parts, true
}

// parseLsfsQAttrs parses the parenthesised attribute line of `lsfs -q`.
// It returns nil if the line carries no size field, which is how a row that
// lsfs could not read is distinguished from one it could.
func parseLsfsQAttrs(line string) *fsAttrs {
	inside := strings.TrimSuffix(strings.TrimPrefix(line, "("), ")")
	var a fsAttrs
	var haveSize bool
	for _, tok := range strings.Split(inside, ",") {
		key, val, ok := strings.Cut(tok, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "block size":
			// JFS2. Wins over frag size if both are somehow present.
			if bs, ok := firstInt(val); ok {
				a.blockSize, haveSize = bs, true
			}
		case "frag size":
			// Old-style JFS (VFS type "jfs") reports "frag size" instead and
			// has no sparse files / inline log / quota attributes at all.
			if !haveSize {
				if bs, ok := firstInt(val); ok {
					a.blockSize, haveSize = bs, true
				}
			}
		case "sparse files":
			a.sparseYes = strings.EqualFold(val, "yes")
		case "inline log":
			a.inlineLog = strings.EqualFold(val, "yes")
		case "quota":
			a.quotaYes = strings.EqualFold(val, "yes")
		}
	}
	if !haveSize {
		return nil
	}
	return &a
}

func parseMountLogDevices(out []byte) map[string]string {
	mp2log := make(map[string]string)
	sc := newScanner(out)
	for sc.Scan() {
		line := sc.Text()
		ltrim := strings.TrimSpace(line)
		if ltrim == "" || strings.HasPrefix(ltrim, "-") || strings.HasPrefix(ltrim, "node") {
			continue
		}
		if !strings.Contains(line, "/dev/") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		mp := parts[1]
		logdev := "UNKNOWN"
		if idx := strings.Index(line, "log="); idx >= 0 {
			val := line[idx+len("log="):]
			cut := len(val)
			if k := strings.IndexAny(val, ", \t"); k >= 0 { // stop at comma/space/tab
				cut = k
			}
			logdev = strings.TrimSpace(val[:cut])
		}
		mp2log[mp] = logdev
	}
	return mp2log
}

// encryptionLookupConcurrency bounds the per-LV lslv fan-out.
const encryptionLookupConcurrency = 4

// encryptionStatus resolves node_filesystem_encryption_enabled for every dev.
//
// No AIX command reports the ENCRYPTION attribute for many logical volumes at
// once, so the naive implementation is one `lslv` fork per mounted filesystem
// — the last remaining 1:N fork multiplier in this collector (rca.md §2c).
// Three layers cut that down, each falling back cleanly to the one below:
//
//  1. A TTL cache (--collector.aix_fsinfo.encryption-cache-ttl). LV encryption
//     is set at provisioning time; re-reading it every 30s scrape is waste.
//  2. A volume group gate. AIX requires the data encryption option to be
//     enabled at the volume group level before it can be enabled on any
//     logical volume inside it, and `hdcryptmgr showvg` (AIX 7.2 TL5 and
//     later, including 7.3) reports that for every VG in a single fork. The
//     LV->VG mapping comes from perfstat.LogicalVolumeStat(), a CGo call with
//     zero forks. On a host where no VG is encryption-enabled — the common
//     case — this answers every LV without ever running lslv, collapsing N
//     forks to 1.
//  3. A bounded worker pool of `lslv` calls for whatever the gate could not
//     rule out, or for everything if hdcryptmgr is not installed (AIX 7.1 /
//     7.2 before TL5), which preserves the previous behaviour exactly.
//
// Devices whose lookup failed are left out of the cache so a transient error
// is not remembered for a whole TTL.
func (c *aixFSInfoCollector) encryptionStatus(ctx context.Context, devs []string) map[string]bool {
	res := make(map[string]bool, len(devs))
	if len(devs) == 0 {
		return res
	}

	ttl := *aixFSInfoEncryptionTTL
	now := time.Now()

	var pending []string
	c.encMu.Lock()
	for _, dev := range devs {
		if e, ok := c.encByDev[dev]; ok && ttl > 0 && now.Sub(e.at) < ttl {
			res[dev] = e.enabled
			continue
		}
		pending = append(pending, dev)
	}
	c.encMu.Unlock()

	fresh := make(map[string]encEntry, len(pending))
	if len(pending) > 0 {
		// Layer 2: rule out whole volume groups without forking per LV.
		if gate := c.cryptoGate(ctx, now, ttl); gate != nil {
			if lvVG := lvToVGPerfstat(c.logger); lvVG != nil {
				var remaining []string
				for _, dev := range pending {
					enabled, known := gate[lvVG[lvNameFromDev(dev)]]
					if known && !enabled {
						res[dev] = false
						fresh[dev] = encEntry{enabled: false, at: now}
						continue
					}
					remaining = append(remaining, dev)
				}
				pending = remaining
			}
		}

		// Layer 3: per-LV lslv for whatever is left.
		for dev, enabled := range encryptionLookupPool(ctx, pending, c.logger) {
			res[dev] = enabled
			fresh[dev] = encEntry{enabled: enabled, at: now}
		}
	}

	// Rebuild the cache from the devices seen this scrape so entries for
	// removed filesystems cannot accumulate.
	c.encMu.Lock()
	next := make(map[string]encEntry, len(devs))
	for _, dev := range devs {
		if e, ok := fresh[dev]; ok {
			next[dev] = e
		} else if e, ok := c.encByDev[dev]; ok {
			next[dev] = e
		}
	}
	c.encByDev = next
	c.encMu.Unlock()

	return res
}

// cryptoGate returns "volume group name -> data encryption enabled", or nil
// when hdcryptmgr is unavailable or its output was unparseable. A VG answering
// "no" definitively rules out every logical volume it contains.
func (c *aixFSInfoCollector) cryptoGate(ctx context.Context, now time.Time, ttl time.Duration) map[string]bool {
	c.encMu.Lock()
	cached := c.vgGate
	c.encMu.Unlock()
	if cached != nil && ttl > 0 && now.Sub(cached.at) < ttl {
		return cached.byVG
	}

	var byVG map[string]bool
	out, err := runCommand(ctx, "hdcryptmgr", "showvg")
	if err != nil {
		// AIX before 7.2 TL5 has no hdcryptmgr at all; a permission or LVM
		// contention failure is equally non-fatal. Either way we fall back to
		// the per-LV path below.
		c.logger.Debug("hdcryptmgr showvg unavailable; using per-LV lslv for encryption status", "err", err)
	} else {
		byVG = parseHdcryptmgrShowVG(out)
	}

	c.encMu.Lock()
	c.vgGate = &vgCryptoGate{byVG: byVG, at: now}
	c.encMu.Unlock()
	return byVG
}

// parseHdcryptmgrShowVG parses `hdcryptmgr showvg`:
//
//	VG NAME / ID         ENCRYPTION ENABLED
//	testvg               yes
//	rootvg               no
//
// The header ends in "ENABLED" rather than yes/no and is skipped by the same
// test that accepts data rows. Returns nil if nothing parsed, so the caller
// treats an unrecognised format as "no gate available" rather than as "no VG
// is encrypted".
func parseHdcryptmgrShowVG(out []byte) map[string]bool {
	res := make(map[string]bool)
	sc := newScanner(out)
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) < 2 {
			continue
		}
		switch strings.ToLower(parts[len(parts)-1]) {
		case "yes":
			res[parts[0]] = true
		case "no":
			res[parts[0]] = false
		}
	}
	if len(res) == 0 {
		return nil
	}
	return res
}

// lvToVGPerfstat maps logical volume name -> volume group name via a single
// perfstat.LogicalVolumeStat() CGo call, with zero subprocess forks. Keying by
// bare LV name is safe here even though LV names are only guaranteed unique
// within a volume group: every LV owns a /dev entry, so two LVs with the same
// name cannot coexist on one host.
func lvToVGPerfstat(logger *slog.Logger) map[string]string {
	lvs, err := perfstat.LogicalVolumeStat()
	if err != nil {
		logger.Debug("perfstat LogicalVolumeStat failed; cannot map LVs to volume groups", "err", err)
		return nil
	}
	m := make(map[string]string, len(lvs))
	for _, lv := range lvs {
		m[lv.Name] = lv.VGName
	}
	return m
}

func lvNameFromDev(dev string) string {
	return strings.TrimPrefix(dev, "/dev/")
}

// lvEncryptionRe matches the ENCRYPTION field lslv reports for an encrypted
// logical volume.
var lvEncryptionRe = regexp.MustCompile(`(?mi)^ENCRYPTION:\s*yes\b`)

// lvEncryptionEnabled reports whether the logical volume backing dev has
// encryption enabled. The error return is distinct from a false result so
// callers can avoid caching a failed lookup.
func lvEncryptionEnabled(ctx context.Context, dev string, logger *slog.Logger) (bool, error) {
	if !strings.HasPrefix(dev, "/dev/") {
		return false, nil
	}
	lv := lvNameFromDev(dev)
	out, err := runCommand(ctx, "lslv", lslvArgs(lv)...)
	if err != nil {
		logger.Debug("lslv failed", "lv", lv, "err", err)
		return false, err
	}
	return lvEncryptionRe.Find(out) != nil, nil
}

// encryptionLookupPool resolves lvEncryptionEnabled for many devices
// concurrently, bounded to encryptionLookupConcurrency workers and to ctx's
// deadline. Devices whose lookup failed are absent from the result.
func encryptionLookupPool(ctx context.Context, devs []string, logger *slog.Logger) map[string]bool {
	results := make(map[string]bool, len(devs))
	if len(devs) == 0 || ctx.Err() != nil {
		return results
	}

	jobs := make(chan string)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i := 0; i < min(encryptionLookupConcurrency, len(devs)); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for dev := range jobs {
				enabled, err := lvEncryptionEnabled(ctx, dev, logger)
				if err != nil {
					continue
				}
				mu.Lock()
				results[dev] = enabled
				mu.Unlock()
			}
		}()
	}

feed:
	for _, dev := range devs {
		select {
		case jobs <- dev:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	return results
}

// ----------------------------------------------------------------------------
// VG/LV Metrics (perfstat + lsvg, lsvg -l)
// Enable with: --collector.aix_vglv
// ----------------------------------------------------------------------------

func init() {
	registerCollector("aix_vglv", defaultDisabled, NewAIXVGLVCollector)
}

type aixVGLVCollector struct {
	vgFreeDesc  *prometheus.Desc // node_vg_free_pps_total{vg}
	vgTotalDesc *prometheus.Desc // node_vg_total_pps{vg}
	lvPPsDesc   *prometheus.Desc // node_lv_pps_total{lv,mountpoint}
	lvStateDesc *prometheus.Desc // node_lv_state{mountpoint,state}
	logger      *slog.Logger
}

func NewAIXVGLVCollector(logger *slog.Logger) (Collector, error) {
	if logger == nil {
		logger = slog.Default()
	}
	// Required once before VolumeGroupStat()/LogicalVolumeStat() return
	// populated data; LVM stats are disabled by default in libperfstat.
	// Safe to call from the collector constructor since NewNodeCollector
	// caches constructed collectors and only calls the factory once per
	// process lifetime (see initiatedCollectors in collector.go).
	perfstat.EnableLVMStat()
	ns := namespace
	return &aixVGLVCollector{
		vgFreeDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "vg", "free_pps_total"),
			"Volume group free physical partitions.",
			[]string{"vg"}, nil,
		),
		vgTotalDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "vg", "total_pps"),
			"Volume group total physical partitions.",
			[]string{"vg"}, nil,
		),
		lvPPsDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "lv", "pps_total"),
			"Logical volume physical partitions in use.",
			[]string{"lv", "mountpoint"}, nil,
		),
		lvStateDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "lv", "state"),
			"Logical volume state reported as info metric (gauge=1).",
			[]string{"mountpoint", "state"}, nil,
		),
		logger: logger,
	}, nil
}

func (c *aixVGLVCollector) Update(ch chan<- prometheus.Metric) error {
	guard := NewTimeoutGuard("aix_vglv", c.logger, 0)
	ctx, cancel := guard.Context()
	defer cancel()

	vgList, err := c.listVolumeGroups(ctx)
	if err != nil {
		if isDeadline(err) {
			guard.FlagTimeout("lsvg_o_timeout")
			guard.EmitIfTimedOut(ch)
		}
		return err
	}

	// Best-effort: cross-populate LV PPs from perfstat (exact-match verified
	// against lsvg -l's PPs column — see findings.md). A nil map (perfstat
	// failed) is handled by the per-row lookup below falling back to the
	// CLI-parsed value, so this never blocks the collector.
	perfstatPPs := fetchPerfstatLVPPs(c.logger)

	results := c.queryVolumeGroups(ctx, vgList)

	// De-dup map for state metrics: key = mountpoint + NUL + state.
	seenState := make(map[string]struct{})
	for _, vg := range vgList {
		r, ok := results[vg]
		if !ok {
			// Never scheduled: the budget ran out while feeding the pool.
			guard.FlagTimeout("lsvg_budget_exhausted")
			continue
		}
		if r.reason != "" {
			guard.FlagTimeout(r.reason)
		}
		if r.haveVG {
			ch <- prometheus.MustNewConstMetric(c.vgFreeDesc, prometheus.GaugeValue, float64(r.freePPs), vg)
			ch <- prometheus.MustNewConstMetric(c.vgTotalDesc, prometheus.GaugeValue, float64(r.totalPPs), vg)
		}
		for _, row := range r.rows {
			pps := row.pps
			if pps == 0 && perfstatPPs != nil {
				// Only a fallback: see fetchPerfstatLVPPs. An LV always has at
				// least one partition, so 0 here means the row did not parse.
				if v, ok := perfstatPPs[perfstatLVKey(vg, row.lv)]; ok {
					pps = v
				}
			}
			ch <- prometheus.MustNewConstMetric(c.lvPPsDesc, prometheus.GaugeValue, float64(pps), row.lv, row.mountpoint)
			if isUnmounted(row.mountpoint) {
				continue
			}
			state := row.state
			if state == "" {
				state = "unknown"
			}
			key := row.mountpoint + "\x00" + state
			if _, dup := seenState[key]; dup {
				continue
			}
			seenState[key] = struct{}{}
			ch <- prometheus.MustNewConstMetric(c.lvStateDesc, prometheus.GaugeValue, 1, row.mountpoint, state)
		}
	}
	guard.EmitIfTimedOut(ch)
	return nil
}

type lvRow struct {
	lv         string
	pps        int64
	state      string
	mountpoint string
}

// vgResult is one volume group's worth of collected data, gathered by a worker
// and emitted by Update so that metric ordering stays deterministic and the
// TimeoutGuard (which is not safe for concurrent use) is only ever touched
// from the collector's own goroutine.
type vgResult struct {
	freePPs  int64
	totalPPs int64
	haveVG   bool
	rows     []lvRow
	reason   string // timeout reason to flag, empty if none
}

// listVolumeGroups returns the varied-on volume groups.
//
// perfstat.VolumeGroupStat() is a single CGo call with zero subprocess forks
// and no timeout risk, so it is tried first; `lsvg -o` is the fallback for
// hosts where libperfstat's LVM statistics are unavailable. An empty perfstat
// result is treated as a failure rather than as "no volume groups": every AIX
// host has at least rootvg varied on, so an empty list means the library
// returned nothing useful and the CLI should answer instead.
func (c *aixVGLVCollector) listVolumeGroups(ctx context.Context) ([]string, error) {
	if vgs, err := listVGsPerfstat(); err != nil {
		c.logger.Debug("perfstat VolumeGroupStat failed; falling back to lsvg -o", "err", err)
	} else if len(vgs) > 0 {
		return vgs, nil
	} else {
		c.logger.Debug("perfstat VolumeGroupStat returned no volume groups; falling back to lsvg -o")
	}

	out, err := runCommand(ctx, "lsvg", lsvgArgs("-o")...)
	if err != nil {
		return nil, err
	}
	var vgs []string
	sc := newScanner(out)
	for sc.Scan() {
		if v := strings.TrimSpace(sc.Text()); v != "" {
			vgs = append(vgs, v)
		}
	}
	return vgs, nil
}

// queryVolumeGroups fans the per-VG lsvg calls out over a bounded pool sharing
// ctx's deadline. Volume groups are independent of one another, so running
// them back to back only served to multiply each call's latency by the VG
// count before the 9s budget was reached.
func (c *aixVGLVCollector) queryVolumeGroups(ctx context.Context, vgs []string) map[string]*vgResult {
	out := make(map[string]*vgResult, len(vgs))
	if len(vgs) == 0 {
		return out
	}

	workers := *aixVGLVConcurrency
	if workers < 1 {
		workers = 1
	}

	jobs := make(chan string)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < min(workers, len(vgs)); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for vg := range jobs {
				r := c.queryVolumeGroup(ctx, vg)
				mu.Lock()
				out[vg] = r
				mu.Unlock()
			}
		}()
	}

feed:
	for _, vg := range vgs {
		select {
		case jobs <- vg:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	return out
}

var (
	lsvgFreePPsRe  = regexp.MustCompile(`FREE PPs:\s+(\d+)`)
	lsvgTotalPPsRe = regexp.MustCompile(`TOTAL PPs:\s+(\d+)`)
)

func (c *aixVGLVCollector) queryVolumeGroup(ctx context.Context, vg string) *vgResult {
	r := &vgResult{}

	if out, err := runCommand(ctx, "lsvg", lsvgArgs(vg)...); err != nil {
		if isDeadline(err) {
			r.reason = "lsvg_timeout"
		} else {
			c.logger.Debug("lsvg vg failed", "vg", vg, "err", err)
		}
	} else {
		// FREE PPs:/TOTAL PPs: appear in the second column of a two-column
		// layout (e.g. "MAX LVs: 256    FREE PPs: 270 (138240 megabytes)"),
		// never at the start of the line — do not anchor with ^.
		r.freePPs = grepInt(lsvgFreePPsRe, out)
		r.totalPPs = grepInt(lsvgTotalPPsRe, out)
		r.haveVG = true
	}

	// lsvg -l still runs regardless of perfstat availability: it is the only
	// source for LV STATE and MOUNT POINT (neither has a perfstat equivalent
	// — see findings.md). Its PPs column remains the fallback value if
	// perfstat has no matching entry.
	if out, err := runCommand(ctx, "lsvg", lsvgArgs("-l", vg)...); err != nil {
		if isDeadline(err) {
			if r.reason == "" {
				r.reason = "lsvg_l_timeout"
			}
		} else {
			c.logger.Debug("lsvg -l failed", "vg", vg, "err", err)
		}
	} else {
		r.rows = parseLsvgDashL(out)
	}

	return r
}

// listVGsPerfstat returns varied-on VG names via perfstat.VolumeGroupStat(),
// a single CGo call with zero subprocess forks — no timeout risk, unlike
// `lsvg -o`. VariedState == 0 means "Available" (varied ON); VariedState == 1
// means "Not Available" (varied OFF). This mapping is documented in IBM's
// libperfstat.h reference and has been empirically confirmed against real
// AIX hosts (9 VGs, zero exceptions across two independent test runs — see
// findings.md at the repo root for the full evidence). Do not change this
// mapping without re-verifying against IBM documentation or a real host;
// getting it backwards would silently make every varied-on VG disappear
// from this collector's output and vice versa.
func listVGsPerfstat() ([]string, error) {
	vgs, err := perfstat.VolumeGroupStat()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, vg := range vgs {
		if vg.VariedState == 0 {
			names = append(names, vg.Name)
		}
	}
	return names, nil
}

// perfstatLVKey builds a lookup key for the VG+LV pair. LV names are only
// guaranteed unique within a volume group, not system-wide, so the key must
// include the VG name.
func perfstatLVKey(vg, lv string) string {
	return vg + "\x00" + lv
}

// fetchPerfstatLVPPs returns a map of "vg\x00lv" -> LogicalPartitions via a
// single perfstat.LogicalVolumeStat() CGo call covering every LV on the host.
// Returns nil (not an error) on failure.
//
// This is a FALLBACK ONLY, used when `lsvg -l`'s PPs column did not parse.
// findings.md concluded LogicalPartitions was an exact match for that column
// and could replace it, but the capture that conclusion rests on
// (Nodeexporter_logs/10.10.5.128) has mirrors == 1 for all 85 logical volumes.
// LogicalPartitions is a count of LOGICAL partitions; the PPs column counts
// PHYSICAL ones, and the two diverge by the copy count as soon as an LV is
// mirrored — a mirrored rootvg would report half its real PPs. `lsvg -l` runs
// on every scrape regardless (it is the only source of LV state and mount
// point), so its PPs column costs nothing extra and stays authoritative.
func fetchPerfstatLVPPs(logger *slog.Logger) map[string]int64 {
	lvs, err := perfstat.LogicalVolumeStat()
	if err != nil {
		logger.Debug("perfstat LogicalVolumeStat failed; node_lv_pps_total will use lsvg -l values only", "err", err)
		return nil
	}
	m := make(map[string]int64, len(lvs))
	for _, lv := range lvs {
		m[perfstatLVKey(lv.VGName, lv.Name)] = lv.LogicalPartitions
	}
	return m
}

// lsvgDashLRowRe matches a `lsvg -l` row. Columns are:
// LV NAME  TYPE  LPs  PPs  PVs  LV STATE  MOUNT POINT
var lsvgDashLRowRe = regexp.MustCompile(`^([A-Za-z0-9_.-]+)\s+\S+\s+\d+\s+(\d+)\s+\d+\s+(\S+)\s+([/A-Za-z0-9_.-]+|N/A|--)$`)

// parseLsvgDashL is a robust parser for `lsvg -l` rows across locales/TLs.
func parseLsvgDashL(out []byte) []lvRow {
	var rows []lvRow
	sc := newScanner(out)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasSuffix(line, ":") || strings.HasPrefix(line, "LV") {
			// Skip blank, VG header lines like "rootvg:" and column header
			continue
		}
		if m := lsvgDashLRowRe.FindStringSubmatch(line); len(m) == 5 {
			pps, _ := strconv.ParseInt(m[2], 10, 64)
			rows = append(rows, lvRow{lv: m[1], pps: pps, state: m[3], mountpoint: m[4]})
			continue
		}
		// Fallback for column drift, using the documented column order rather
		// than counting back from the end: a row without a MOUNT POINT column
		// would otherwise take PVs as its state.
		parts := strings.Fields(line)
		if len(parts) >= 6 {
			row := lvRow{lv: parts[0], state: parts[5]}
			if pps, err := strconv.ParseInt(parts[3], 10, 64); err == nil {
				row.pps = pps
			}
			if len(parts) >= 7 {
				row.mountpoint = parts[len(parts)-1]
			}
			rows = append(rows, row)
		}
	}
	return rows
}

// ----------------------------------------------------------------------------
// Buffer Pressure (vmstat -v)
// Enable with: --collector.aix_vmstat_fsbuf
// ----------------------------------------------------------------------------

func init() {
	registerCollector("aix_vmstat_fsbuf", defaultDisabled, NewAIXVMStatFSBufCollector)
}

type aixVMStatFSBufCollector struct {
	fsbufDesc       *prometheus.Desc // node_jfs2_fsbuf_blocked_total
	clientFsbufDesc *prometheus.Desc // node_jfs2_client_fsbuf_blocked_total
	extPagerDesc    *prometheus.Desc // node_jfs2_external_pager_fsbuf_blocked_total
	logger          *slog.Logger
}

func NewAIXVMStatFSBufCollector(logger *slog.Logger) (Collector, error) {
	if logger == nil {
		logger = slog.Default()
	}
	ns := namespace
	return &aixVMStatFSBufCollector{
		fsbufDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "jfs2", "fsbuf_blocked_total"),
			"Filesystem I/Os blocked with no fsbuf (vmstat -v, cumulative since boot).",
			nil, nil,
		),
		clientFsbufDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "jfs2", "client_fsbuf_blocked_total"),
			"Client filesystem I/Os blocked with no fsbuf (vmstat -v, cumulative since boot).",
			nil, nil,
		),
		extPagerDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "jfs2", "external_pager_fsbuf_blocked_total"),
			"External pager filesystem I/Os blocked with no fsbuf (vmstat -v, cumulative since boot). This is the JFS2 counter; the plain fsbuf counter is JFS.",
			nil, nil,
		),
		logger: logger,
	}, nil
}

// vmstat -v prints the counter BEFORE its label, one statistic per line:
//
//	2740  filesystem I/Os blocked with no fsbuf
//	   0  client filesystem I/Os blocked with no fsbuf
//	   0  external pager filesystem I/Os blocked with no fsbuf
//
// These patterns are anchored at both ends so that the plain counter cannot
// match the "client"/"external pager" lines and vice versa.
var (
	vmstatFSBufRe       = regexp.MustCompile(`(?m)^\s*(\d+)\s+filesystem I/Os blocked with no fsbuf\s*$`)
	vmstatClientFSBufRe = regexp.MustCompile(`(?m)^\s*(\d+)\s+client filesystem I/Os blocked with no fsbuf\s*$`)
	vmstatExtPagerRe    = regexp.MustCompile(`(?m)^\s*(\d+)\s+external pager filesystem I/Os blocked with no fsbuf\s*$`)
)

func (c *aixVMStatFSBufCollector) Update(ch chan<- prometheus.Metric) error {
	guard := NewTimeoutGuard("aix_vmstat_fsbuf", c.logger, 0)
	ctx, cancel := guard.Context()
	defer cancel()

	out, err := runCommand(ctx, "vmstat", "-v")
	if err != nil {
		if isDeadline(err) {
			guard.FlagTimeout("vmstat_v_timeout")
			guard.EmitIfTimedOut(ch)
		}
		return fmt.Errorf("vmstat -v: %w", err)
	}
	ch <- prometheus.MustNewConstMetric(c.fsbufDesc, prometheus.CounterValue, float64(grepInt(vmstatFSBufRe, out)))
	ch <- prometheus.MustNewConstMetric(c.clientFsbufDesc, prometheus.CounterValue, float64(grepInt(vmstatClientFSBufRe, out)))
	ch <- prometheus.MustNewConstMetric(c.extPagerDesc, prometheus.CounterValue, float64(grepInt(vmstatExtPagerRe, out)))
	return nil
}

// ----------------------------------------------------------------------------
// NFS via nfsstat -s / -c
// Enable with: --collector.aix_nfsstat
// ----------------------------------------------------------------------------

func init() {
	registerCollector("aix_nfsstat", defaultDisabled, NewAIXNFSStatCollector)
}

type aixNFSStatCollector struct {
	srvCallsDesc *prometheus.Desc // node_nfs_server_calls_total
	cliCallsDesc *prometheus.Desc // node_nfs_client_calls_total
	srvOpsDesc   *prometheus.Desc // node_nfs_server_ops_total{op}
	logger       *slog.Logger
}

func NewAIXNFSStatCollector(logger *slog.Logger) (Collector, error) {
	if logger == nil {
		logger = slog.Default()
	}
	ns := namespace
	return &aixNFSStatCollector{
		srvCallsDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "nfs", "server_calls_total"),
			"NFS server total calls (from nfsstat -s).",
			nil, nil,
		),
		cliCallsDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "nfs", "client_calls_total"),
			"NFS client total calls (from nfsstat -c).",
			nil, nil,
		),
		srvOpsDesc: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "nfs", "server_ops_total"),
			"NFS server calls by operation (v2/v3/v4) from nfsstat -s.",
			[]string{"op"}, nil,
		),
		logger: logger,
	}, nil
}

func (c *aixNFSStatCollector) Update(ch chan<- prometheus.Metric) error {
	// nfsstat can stall on a host with unreachable NFS mounts, so it runs on
	// the same shared budget as the other collectors in this file rather than
	// on an unaccounted fixed timeout.
	guard := NewTimeoutGuard("aix_nfsstat", c.logger, 0)
	ctx, cancel := guard.Context()
	defer cancel()

	// Server stats
	srvOut, err := runCommand(ctx, "nfsstat", "-s")
	if err == nil {
		srvCalls := parseServerCallsAIXAny(srvOut)
		ch <- prometheus.MustNewConstMetric(c.srvCallsDesc, prometheus.CounterValue, float64(srvCalls))
		for op, val := range parseNfsstatOpsAny(srvOut) {
			ch <- prometheus.MustNewConstMetric(c.srvOpsDesc, prometheus.CounterValue, float64(val), op)
		}
	} else {
		if isDeadline(err) {
			guard.FlagTimeout("nfsstat_s_timeout")
		}
		c.logger.Debug("nfsstat -s failed", "err", err)
	}

	// Client stats (prefer connection-oriented, fallback to connectionless)
	cliOut, err := runCommand(ctx, "nfsstat", "-c")
	if err == nil {
		cliCalls := parseClientCallsAIX(cliOut)
		ch <- prometheus.MustNewConstMetric(c.cliCallsDesc, prometheus.CounterValue, float64(cliCalls))
	} else {
		if isDeadline(err) {
			guard.FlagTimeout("nfsstat_c_timeout")
		}
		c.logger.Debug("nfsstat -c failed", "err", err)
	}
	guard.EmitIfTimedOut(ch)
	return nil
}

// parseServerCallsAIXAny extracts "calls" from any of the AIX server tables
// (Server nfs:, Server nfs v2:, v3:, v4:). It returns the first parsable value.
func parseServerCallsAIXAny(out []byte) int64 {
	sc := newScanner(out)
	in := false
	header := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "Server nfs:") ||
			strings.HasPrefix(line, "Server nfs v2:") ||
			strings.HasPrefix(line, "Server nfs v3:") ||
			strings.HasPrefix(line, "Server nfs v4:") {
			in = true
			header = false
			continue
		}
		if !in {
			continue
		}
		l := strings.ToLower(line)
		if !header && strings.HasPrefix(l, "calls") {
			header = true
			continue
		}
		if header {
			parts := strings.Fields(line)
			if len(parts) > 0 {
				if v, err := strconv.ParseInt(parts[0], 10, 64); err == nil {
					return v
				}
			}
			// Stop at first numbers row even if unparsable; continue scanning for other sections
			in = false
			header = false
		}
	}
	return 0
}

// parseClientCallsAIX extracts client calls from "Client rpc:" tables.
// Prefer "Connection oriented", else use "Connectionless"; tolerate missing sections.
func parseClientCallsAIX(out []byte) int64 {
	sc := newScanner(out)
	inClient := false
	section := "" // "oriented" or "connectionless"
	header := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "Client rpc:") {
			inClient = true
			section = ""
			header = false
			continue
		}
		if !inClient {
			continue
		}
		l := strings.ToLower(line)
		if section == "" {
			if strings.HasPrefix(l, "connection oriented") {
				section = "oriented"
				header = false
				continue
			}
			if strings.HasPrefix(l, "connectionless") {
				section = "connectionless"
				header = false
				continue
			}
			continue
		}
		if !header && strings.HasPrefix(l, "calls") {
			header = true
			continue
		}
		if header {
			parts := strings.Fields(line)
			if len(parts) > 0 {
				if v, err := strconv.ParseInt(parts[0], 10, 64); err == nil {
					return v
				}
			}
			if section == "oriented" { // try connectionless next
				section = ""
				header = false
				continue
			}
			break
		}
	}
	return 0
}

// parseNfsstatOpsAny parses AIX "Version 2/3/4" blocks in nfsstat -s output.
// It collects op counts by pairing op-name lines with number/% lines.
// Counts are summed across versions for identical op names.
func parseNfsstatOpsAny(out []byte) map[string]int64 {
	ops := make(map[string]int64)
	sc := newScanner(out)
	inVersion := false
	var names []string
	isAlpha := func(s string) bool {
		if s == "" {
			return false
		}
		for _, r := range s {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '+' || r == '_' {
				continue
			}
			return false
		}
		return true
	}
	isInt := func(s string) bool {
		_, err := strconv.ParseInt(s, 10, 64)
		return err == nil
	}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "Version 2:") || strings.HasPrefix(line, "Version 3:") || strings.HasPrefix(line, "Version 4:") {
			inVersion = true
			names = nil
			continue
		}
		if !inVersion {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}
		if isAlpha(parts[0]) {
			names = nil
			for _, p := range parts {
				if isAlpha(p) {
					names = append(names, strings.ToLower(p))
				}
			}
			continue
		}
		if len(names) > 0 && isInt(parts[0]) {
			idx := 0
			for i := 0; i < len(parts); i += 2 {
				if idx >= len(names) {
					break
				}
				if cnt, err := strconv.ParseInt(parts[i], 10, 64); err == nil && cnt > 0 {
					ops[names[idx]] += cnt
				}
				idx++
			}
		}
	}
	return ops
}
