// Package cachetool implements bounded, auditable package-cache reclamation
// backed by official package-manager CLIs (starting with uv).
package cachetool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/fsops"
)

const (
	ToolUV         = "uv"
	ledgerFileName = "cache-actions.json"
	ledgerVersion  = 1
)

type Options struct {
	InstanceID string
	RunID      string
	StateDir   string
	Tool       string
	BinaryPath string
	CacheDir   string
	ToolDir    string
	Digest     string
	MaxBytes   int64
	NowNS      func() int64
	// Test-only seam invoked right after the durable pending intent is persisted.
	AfterIntentHook func(opID string) error
}

type EnvTarget struct {
	Name   string `json:"name"`
	Link   string `json:"link"`
	Target string `json:"target"`
}

type PreviewResult struct {
	Tool                     string        `json:"tool"`
	BinaryPath               string        `json:"binary_path"`
	ToolVersion              string        `json:"tool_version"`
	CacheRoot                string        `json:"cache_root"`
	CacheVolumeID            string        `json:"cache_volume_id"`
	CacheDevice              uint64        `json:"cache_device"`
	CacheInode               uint64        `json:"cache_inode"`
	ToolDir                  string        `json:"tool_dir,omitempty"`
	ProtectedEnvs            []EnvTarget   `json:"protected_envs"`
	Files                    int           `json:"files"`
	LogicalBytes             int64         `json:"logical_bytes"`
	UpperBoundAllocatedBytes int64         `json:"upper_bound_allocated_bytes"`
	ReclaimLowerBoundKnown   bool          `json:"reclaim_lower_bound_known"`
	MinimumReclaimGuaranteed int64         `json:"minimum_reclaim_guaranteed"`
	VolumeAvailBytes         int64         `json:"volume_avail_bytes"`
	Digest                   domain.Digest `json:"digest"`
	Notice                   string        `json:"notice"`
}

type ActionRecord struct {
	OpID                    string      `json:"op_id"`
	RunID                   string      `json:"run_id"`
	InstanceID              string      `json:"instance_id"`
	Tool                    string      `json:"tool"`
	BinaryPath              string      `json:"binary_path"`
	ToolVersion             string      `json:"tool_version"`
	CacheRoot               string      `json:"cache_root"`
	CacheVolumeID           string      `json:"cache_volume_id"`
	CacheDevice             uint64      `json:"cache_device"`
	CacheInode              uint64      `json:"cache_inode"`
	ToolDir                 string      `json:"tool_dir,omitempty"`
	Digest                  string      `json:"digest"`
	State                   string      `json:"state"` // pending | completed | failed | reconciled
	StartedAtNS             int64       `json:"started_at_ns"`
	CompletedAtNS           int64       `json:"completed_at_ns,omitempty"`
	PreFiles                int         `json:"pre_files"`
	PreLogicalBytes         int64       `json:"pre_logical_bytes"`
	PreAllocatedBytes       int64       `json:"pre_allocated_bytes"`
	PreLatestMTimeNS        int64       `json:"pre_latest_mtime_ns"`
	PreVolAvailBytes        int64       `json:"pre_vol_avail_bytes"`
	PostFiles               int         `json:"post_files"`
	PostLogicalBytes        int64       `json:"post_logical_bytes"`
	PostAllocatedBytes      int64       `json:"post_allocated_bytes"`
	PostLatestMTimeNS       int64       `json:"post_latest_mtime_ns"`
	PostVolAvailBytes       int64       `json:"post_vol_avail_bytes"`
	ReclaimedAllocatedBytes int64       `json:"reclaimed_allocated_bytes"`
	ReclaimedLogicalBytes   int64       `json:"reclaimed_logical_bytes"`
	VolAvailDeltaBytes      int64       `json:"vol_avail_delta_bytes"`
	ToolReportedBytes       *int64      `json:"tool_reported_bytes,omitempty"`
	ToolReportedSummary     string      `json:"tool_reported_summary,omitempty"`
	ProtectedEnvs           []EnvTarget `json:"protected_envs,omitempty"`
	ProtectedEnvsIntact     bool        `json:"protected_envs_intact"`
	ZeroAction              bool        `json:"zero_action,omitempty"`
	ZeroActionReason        string      `json:"zero_action_reason,omitempty"`
	Note                    string      `json:"note,omitempty"`
}

type LedgerStatus struct {
	Records   []ActionRecord `json:"records"`
	PendingOp *ActionRecord  `json:"pending_op,omitempty"`
}

type ledgerFile struct {
	Version int            `json:"version"`
	Records []ActionRecord `json:"records"`
}

type uvInspection struct {
	binPath       string
	binDev        uint64
	binIno        uint64
	binSize       int64
	binMTimeNS    int64
	version       string
	cacheRoot     string
	cacheVolID    string
	cacheDev      uint64
	cacheIno      uint64
	toolDir       string
	protectedEnvs []EnvTarget
	stats         treeStats
	volAvail      int64
	digest        domain.Digest
}

type treeStats struct {
	files         int
	logicalBytes  int64
	allocatedByte int64
	latestMTimeNS int64
}

type digestInput struct {
	Version       int    `json:"version"`
	InstanceID    string `json:"instance_id"`
	Tool          string `json:"tool"`
	BinaryPath    string `json:"binary_path"`
	BinaryDev     uint64 `json:"binary_dev"`
	BinaryIno     uint64 `json:"binary_ino"`
	BinarySize    int64  `json:"binary_size"`
	BinaryMTimeNS int64  `json:"binary_mtime_ns"`
	ToolVersion   string `json:"tool_version"`
	CacheRoot     string `json:"cache_root"`
	CacheVolumeID string `json:"cache_volume_id"`
	CacheInode    uint64 `json:"cache_inode"`
	ToolDir       string `json:"tool_dir"`
	MaxBytes      int64  `json:"max_bytes"`
}

func Preview(ctx context.Context, opt Options) (PreviewResult, error) {
	ins, err := inspectUV(ctx, opt)
	if err != nil {
		return PreviewResult{}, err
	}
	return PreviewResult{
		Tool:                     ToolUV,
		BinaryPath:               ins.binPath,
		ToolVersion:              ins.version,
		CacheRoot:                ins.cacheRoot,
		CacheVolumeID:            ins.cacheVolID,
		CacheDevice:              ins.cacheDev,
		CacheInode:               ins.cacheIno,
		ToolDir:                  ins.toolDir,
		ProtectedEnvs:            ins.protectedEnvs,
		Files:                    ins.stats.files,
		LogicalBytes:             ins.stats.logicalBytes,
		UpperBoundAllocatedBytes: ins.stats.allocatedByte,
		ReclaimLowerBoundKnown:   false,
		MinimumReclaimGuaranteed: 0,
		VolumeAvailBytes:         ins.volAvail,
		Digest:                   ins.digest,
		Notice:                   "uv cache prune has no dry-run mode; allocated bytes represent the whole cache upper bound, minimum reclaim is unknown, and installed uv tool environments are verified intact",
	}, nil
}

func Prune(ctx context.Context, opt Options) (ActionRecord, error) {
	if strings.TrimSpace(opt.StateDir) == "" {
		return ActionRecord{}, errs.New(errs.CodeInvalidArgument, "state directory is required")
	}
	if strings.TrimSpace(opt.InstanceID) == "" {
		return ActionRecord{}, errs.New(errs.CodeInvalidArgument, "instance ID is required")
	}
	if strings.TrimSpace(opt.Digest) == "" {
		return ActionRecord{}, errs.New(errs.CodeNeedsInput, "missing --digest; run `dustkeeper cache preview --tool uv` first")
	}
	nowFn := opt.NowNS
	if nowFn == nil {
		nowFn = func() int64 { return time.Now().UnixNano() }
	}

	led, err := loadLedger(opt.StateDir)
	if err != nil {
		return ActionRecord{}, err
	}
	for i := range led.Records {
		if led.Records[i].State == "pending" {
			return ActionRecord{}, errs.Newf(errs.CodeRecoveryRequired,
				"cache operation %s is unresolved (pending); inspect and run `dustkeeper cache reconcile --op %s --confirm` instead of blind-retrying",
				led.Records[i].OpID, led.Records[i].OpID)
		}
	}

	ins, err := inspectUV(ctx, opt)
	if err != nil {
		return ActionRecord{}, err
	}
	if domain.Digest(opt.Digest) != ins.digest {
		return ActionRecord{}, errs.Newf(errs.CodeDigestMismatch,
			"confirmation digest does not match current uv binary/cache state (expected %s); preview again", ins.digest)
	}
	if opt.MaxBytes > 0 && ins.stats.allocatedByte > opt.MaxBytes {
		return ActionRecord{}, errs.Newf(errs.CodeBudgetExhausted,
			"uv cache prune acts on the whole cache (%d allocated bytes), which exceeds --max-bytes (%d); raise --max-bytes or preview without a lower cap",
			ins.stats.allocatedByte, opt.MaxBytes)
	}

	// Second normal run with an unchanged cache after a completed prune becomes an explainable zero-action.
	if prevSettled := lastCompletedFor(&led, ins); prevSettled != nil {
		if ins.stats.files == prevSettled.PostFiles &&
			ins.stats.allocatedByte == prevSettled.PostAllocatedBytes &&
			ins.stats.logicalBytes == prevSettled.PostLogicalBytes &&
			ins.stats.latestMTimeNS == prevSettled.PostLatestMTimeNS {
			ts := nowFn()
			rec := ActionRecord{
				OpID:                domain.NewID("cop"),
				RunID:               opt.RunID,
				InstanceID:          opt.InstanceID,
				Tool:                ToolUV,
				BinaryPath:          ins.binPath,
				ToolVersion:         ins.version,
				CacheRoot:           ins.cacheRoot,
				CacheVolumeID:       ins.cacheVolID,
				CacheDevice:         ins.cacheDev,
				CacheInode:          ins.cacheIno,
				ToolDir:             ins.toolDir,
				Digest:              string(ins.digest),
				State:               "completed",
				StartedAtNS:         ts,
				CompletedAtNS:       ts,
				PreFiles:            ins.stats.files,
				PreLogicalBytes:     ins.stats.logicalBytes,
				PreAllocatedBytes:   ins.stats.allocatedByte,
				PreLatestMTimeNS:    ins.stats.latestMTimeNS,
				PreVolAvailBytes:    ins.volAvail,
				PostFiles:           ins.stats.files,
				PostLogicalBytes:    ins.stats.logicalBytes,
				PostAllocatedBytes:  ins.stats.allocatedByte,
				PostLatestMTimeNS:   ins.stats.latestMTimeNS,
				PostVolAvailBytes:   ins.volAvail,
				ProtectedEnvs:       ins.protectedEnvs,
				ProtectedEnvsIntact: verifyProtectedEnvs(ins.protectedEnvs),
				ZeroAction:          true,
				ZeroActionReason:    fmt.Sprintf("cache unchanged since settled operation %s; skipped spawning uv", prevSettled.OpID),
			}
			led.Records = append(led.Records, rec)
			if err := saveLedger(opt.StateDir, led); err != nil {
				return ActionRecord{}, err
			}
			return rec, nil
		}
	}

	if ins.stats.files == 0 {
		ts := nowFn()
		rec := ActionRecord{
			OpID:                domain.NewID("cop"),
			RunID:               opt.RunID,
			InstanceID:          opt.InstanceID,
			Tool:                ToolUV,
			BinaryPath:          ins.binPath,
			ToolVersion:         ins.version,
			CacheRoot:           ins.cacheRoot,
			CacheVolumeID:       ins.cacheVolID,
			CacheDevice:         ins.cacheDev,
			CacheInode:          ins.cacheIno,
			ToolDir:             ins.toolDir,
			Digest:              string(ins.digest),
			State:               "completed",
			StartedAtNS:         ts,
			CompletedAtNS:       ts,
			PreVolAvailBytes:    ins.volAvail,
			PostVolAvailBytes:   ins.volAvail,
			ProtectedEnvs:       ins.protectedEnvs,
			ProtectedEnvsIntact: verifyProtectedEnvs(ins.protectedEnvs),
			ZeroAction:          true,
			ZeroActionReason:    "uv cache directory is already empty",
		}
		led.Records = append(led.Records, rec)
		if err := saveLedger(opt.StateDir, led); err != nil {
			return ActionRecord{}, err
		}
		return rec, nil
	}

	// Persist durable pending intent BEFORE invoking the external tool.
	opID := domain.NewID("cop")
	intent := ActionRecord{
		OpID:              opID,
		RunID:             opt.RunID,
		InstanceID:        opt.InstanceID,
		Tool:              ToolUV,
		BinaryPath:        ins.binPath,
		ToolVersion:       ins.version,
		CacheRoot:         ins.cacheRoot,
		CacheVolumeID:     ins.cacheVolID,
		CacheDevice:       ins.cacheDev,
		CacheInode:        ins.cacheIno,
		ToolDir:           ins.toolDir,
		Digest:            string(ins.digest),
		State:             "pending",
		StartedAtNS:       nowFn(),
		PreFiles:          ins.stats.files,
		PreLogicalBytes:   ins.stats.logicalBytes,
		PreAllocatedBytes: ins.stats.allocatedByte,
		PreLatestMTimeNS:  ins.stats.latestMTimeNS,
		PreVolAvailBytes:  ins.volAvail,
		ProtectedEnvs:     ins.protectedEnvs,
	}
	led.Records = append(led.Records, intent)
	idx := len(led.Records) - 1
	if err := saveLedger(opt.StateDir, led); err != nil {
		return ActionRecord{}, err
	}

	if opt.AfterIntentHook != nil {
		if herr := opt.AfterIntentHook(opID); herr != nil {
			return intent, errs.Wrap(errs.CodeRecoveryRequired, herr)
		}
	}

	// Re-verify binary and cache root identity right before execution.
	var bst unix.Stat_t
	if err := unix.Lstat(ins.binPath, &bst); err != nil || uint64(bst.Dev) != ins.binDev || bst.Ino != ins.binIno {
		led.Records[idx].State = "failed"
		led.Records[idx].CompletedAtNS = nowFn()
		led.Records[idx].Note = "uv binary changed before execution; nothing spawned"
		_ = saveLedger(opt.StateDir, led)
		return ActionRecord{}, errs.New(errs.CodeScopeChanged, "uv binary changed before execution")
	}

	execCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	outText, runErr := runUVCommand(execCtx, ins.binPath, ins.cacheRoot, "cache", "prune", "--cache-dir", ins.cacheRoot)

	postStats, postAvail, postErr := measureCacheRoot(ins.cacheRoot, ins.cacheDev, ins.cacheIno)
	envsIntact := verifyProtectedEnvs(ins.protectedEnvs)

	rec := &led.Records[idx]
	rec.CompletedAtNS = nowFn()
	rec.ProtectedEnvsIntact = envsIntact
	rec.ToolReportedSummary = compactOutput(outText)
	rec.ToolReportedBytes = parseUVReportedBytes(outText)

	if postErr == nil {
		rec.PostFiles = postStats.files
		rec.PostLogicalBytes = postStats.logicalBytes
		rec.PostAllocatedBytes = postStats.allocatedByte
		rec.PostLatestMTimeNS = postStats.latestMTimeNS
		rec.PostVolAvailBytes = postAvail
		if rec.PreAllocatedBytes > rec.PostAllocatedBytes {
			rec.ReclaimedAllocatedBytes = rec.PreAllocatedBytes - rec.PostAllocatedBytes
		}
		if rec.PreLogicalBytes > rec.PostLogicalBytes {
			rec.ReclaimedLogicalBytes = rec.PreLogicalBytes - rec.PostLogicalBytes
		}
		rec.VolAvailDeltaBytes = rec.PostVolAvailBytes - rec.PreVolAvailBytes
	}

	if runErr != nil || postErr != nil || !envsIntact {
		// Keep pending when external execution may have partially mutated the cache or damaged envs.
		reasons := []string{}
		if runErr != nil {
			reasons = append(reasons, "uv exited with error: "+compactOutput(runErr.Error()))
		}
		if postErr != nil {
			reasons = append(reasons, "post-readback failed: "+postErr.Error())
		}
		if !envsIntact {
			reasons = append(reasons, "one or more protected uv tool environments failed post-verification")
		}
		rec.Note = strings.Join(reasons, "; ")
		_ = saveLedger(opt.StateDir, led)
		return *rec, errs.Newf(errs.CodeRecoveryRequired, "uv cache prune did not settle cleanly (%s); inspect `%s` and reconcile op %s", rec.Note, ins.cacheRoot, rec.OpID)
	}

	rec.State = "completed"
	if err := saveLedger(opt.StateDir, led); err != nil {
		return *rec, err
	}
	_ = writeCacheReportSnapshot(opt.StateDir, *rec)
	return *rec, nil
}

func Status(stateDir string) (LedgerStatus, error) {
	led, err := loadLedger(stateDir)
	if err != nil {
		return LedgerStatus{}, err
	}
	out := LedgerStatus{Records: led.Records}
	if out.Records == nil {
		out.Records = []ActionRecord{}
	}
	for i := range out.Records {
		if out.Records[i].State == "pending" {
			cp := out.Records[i]
			out.PendingOp = &cp
			break
		}
	}
	return out, nil
}

func Reconcile(ctx context.Context, stateDir, instanceID, opID, note string, nowNS func() int64) (ActionRecord, error) {
	_ = ctx
	if strings.TrimSpace(opID) == "" {
		return ActionRecord{}, errs.New(errs.CodeInvalidArgument, "--op is required")
	}
	if nowNS == nil {
		nowNS = func() int64 { return time.Now().UnixNano() }
	}
	led, err := loadLedger(stateDir)
	if err != nil {
		return ActionRecord{}, err
	}
	idx := -1
	for i := range led.Records {
		if led.Records[i].OpID == opID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ActionRecord{}, errs.Newf(errs.CodeItemNotFound, "no cache operation %q found", opID)
	}
	rec := &led.Records[idx]
	if instanceID != "" && rec.InstanceID != "" && rec.InstanceID != instanceID {
		return ActionRecord{}, errs.New(errs.CodeScopeChanged, "cache operation belongs to a different instance")
	}
	if rec.State != "pending" {
		return *rec, nil
	}

	postStats, postAvail, postErr := measureCacheRoot(rec.CacheRoot, rec.CacheDevice, rec.CacheInode)
	if postErr == nil {
		rec.PostFiles = postStats.files
		rec.PostLogicalBytes = postStats.logicalBytes
		rec.PostAllocatedBytes = postStats.allocatedByte
		rec.PostLatestMTimeNS = postStats.latestMTimeNS
		rec.PostVolAvailBytes = postAvail
		if rec.PreAllocatedBytes > rec.PostAllocatedBytes {
			rec.ReclaimedAllocatedBytes = rec.PreAllocatedBytes - rec.PostAllocatedBytes
		}
		if rec.PreLogicalBytes > rec.PostLogicalBytes {
			rec.ReclaimedLogicalBytes = rec.PreLogicalBytes - rec.PostLogicalBytes
		}
		rec.VolAvailDeltaBytes = rec.PostVolAvailBytes - rec.PreVolAvailBytes
	}
	rec.ProtectedEnvsIntact = verifyProtectedEnvs(rec.ProtectedEnvs)
	rec.State = "reconciled"
	rec.CompletedAtNS = nowNS()
	if strings.TrimSpace(note) != "" {
		rec.Note = strings.TrimSpace(note)
	} else if rec.Note == "" {
		rec.Note = "operator reconciled interrupted cache operation after post-state check"
	}
	if err := saveLedger(stateDir, led); err != nil {
		return ActionRecord{}, err
	}
	return *rec, nil
}

// ---- internal helpers -----------------------------------------------------

func inspectUV(ctx context.Context, opt Options) (uvInspection, error) {
	tool := strings.TrimSpace(opt.Tool)
	if tool == "" {
		tool = ToolUV
	}
	if tool != ToolUV {
		return uvInspection{}, errs.Newf(errs.CodeInvalidArgument, "unsupported cache tool %q; supported: uv", tool)
	}
	if opt.MaxBytes < 0 {
		return uvInspection{}, errs.New(errs.CodeInvalidArgument, "--max-bytes must be >= 0")
	}

	rawBin := strings.TrimSpace(opt.BinaryPath)
	if rawBin == "" {
		found, err := exec.LookPath("uv")
		if err != nil {
			return uvInspection{}, errs.New(errs.CodeUnsupportedMetadata, "uv is not installed on PATH; install uv or supply --bin")
		}
		rawBin = found
	}
	binPath, err := filepath.EvalSymlinks(rawBin)
	if err != nil {
		return uvInspection{}, errs.Wrap(errs.CodeAccessDenied, err)
	}
	binPath, err = filepath.Abs(binPath)
	if err != nil {
		return uvInspection{}, errs.Wrap(errs.CodeInvalidArgument, err)
	}
	var bst unix.Stat_t
	if err := unix.Lstat(binPath, &bst); err != nil {
		return uvInspection{}, errs.Wrap(errs.CodeAccessDenied, err)
	}
	if bst.Mode&unix.S_IFMT != unix.S_IFREG || bst.Mode&0o111 == 0 {
		return uvInspection{}, errs.Newf(errs.CodeInvalidArgument, "%q is not an executable regular file", binPath)
	}
	if bst.Mode&0o002 != 0 {
		return uvInspection{}, errs.Newf(errs.CodeScopeRefused, "refusing world-writable binary %q", binPath)
	}

	qCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	verOut, err := runUVCommand(qCtx, binPath, "", "--version")
	if err != nil {
		return uvInspection{}, errs.Wrapf(errs.CodeIOError, err, "uv --version failed")
	}
	ver := compactOutput(verOut)
	if !strings.HasPrefix(ver, "uv ") {
		return uvInspection{}, errs.Newf(errs.CodeInvalidArgument, "unexpected uv --version output %q", ver)
	}

	rawCache := strings.TrimSpace(opt.CacheDir)
	if rawCache == "" {
		cOut, err := runUVCommand(qCtx, binPath, "", "cache", "dir")
		if err != nil {
			return uvInspection{}, errs.Wrapf(errs.CodeIOError, err, "uv cache dir failed")
		}
		rawCache = strings.TrimSpace(cOut)
	}
	absCache, err := filepath.Abs(rawCache)
	if err != nil || rawCache == "" {
		return uvInspection{}, errs.New(errs.CodeInvalidArgument, "invalid uv cache directory")
	}
	var clst unix.Stat_t
	if err := unix.Lstat(absCache, &clst); err != nil {
		return uvInspection{}, errs.Wrapf(errs.CodeAccessDenied, err, "uv cache directory %q unavailable", absCache)
	}
	if clst.Mode&unix.S_IFMT == unix.S_IFLNK {
		return uvInspection{}, errs.Newf(errs.CodeScopeRefused, "uv cache directory %q must not be a symbolic link", absCache)
	}
	canonCache, err := filepath.EvalSymlinks(absCache)
	if err != nil {
		return uvInspection{}, errs.Wrap(errs.CodeAccessDenied, err)
	}
	if err := refuseBroadRoot(canonCache); err != nil {
		return uvInspection{}, err
	}

	cRoot, err := fsops.OpenRoot(canonCache)
	if err != nil {
		return uvInspection{}, errs.Wrap(errs.CodeAccessDenied, err)
	}
	rootID, ierr := cRoot.Identity()
	cDev := cRoot.Dev()
	cRoot.Close()
	if ierr != nil {
		return uvInspection{}, errs.Wrap(errs.CodeIOError, ierr)
	}
	if rootID.Persistent.OwnerUID != uint32(os.Getuid()) {
		return uvInspection{}, errs.Newf(errs.CodeScopeRefused, "uv cache directory %q is owned by uid %d, not current uid %d",
			canonCache, rootID.Persistent.OwnerUID, os.Getuid())
	}

	rawToolDir := strings.TrimSpace(opt.ToolDir)
	if rawToolDir == "" {
		if tOut, terr := runUVCommand(qCtx, binPath, "", "tool", "dir"); terr == nil {
			rawToolDir = strings.TrimSpace(tOut)
		}
	}
	canonToolDir := ""
	if rawToolDir != "" {
		if absT, err := filepath.Abs(rawToolDir); err == nil {
			if evalT, err := filepath.EvalSymlinks(absT); err == nil {
				canonToolDir = evalT
			} else {
				canonToolDir = absT
			}
		}
	}
	if canonToolDir != "" && pathsOverlap(canonCache, canonToolDir) {
		return uvInspection{}, errs.Newf(errs.CodeScopeRefused,
			"uv cache root %q overlaps uv tool environment root %q; refusing indistinguishable cache/tool root",
			canonCache, canonToolDir)
	}

	envs := discoverProtectedEnvs(canonToolDir)
	stats, volAvail, err := measureCacheRoot(canonCache, cDev, rootID.Persistent.Inode)
	if err != nil {
		return uvInspection{}, err
	}

	di := digestInput{
		Version:       1,
		InstanceID:    opt.InstanceID,
		Tool:          ToolUV,
		BinaryPath:    binPath,
		BinaryDev:     uint64(bst.Dev),
		BinaryIno:     bst.Ino,
		BinarySize:    bst.Size,
		BinaryMTimeNS: time.Unix(bst.Mtim.Sec, bst.Mtim.Nsec).UnixNano(),
		ToolVersion:   ver,
		CacheRoot:     canonCache,
		CacheVolumeID: rootID.Persistent.VolumeID,
		CacheInode:    rootID.Persistent.Inode,
		ToolDir:       canonToolDir,
		MaxBytes:      opt.MaxBytes,
	}
	rawDigest, err := json.Marshal(di)
	if err != nil {
		return uvInspection{}, errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	sum := domain.DigestOf("dustkeeper/cache-prune/v1", rawDigest)

	return uvInspection{
		binPath:       binPath,
		binDev:        uint64(bst.Dev),
		binIno:        bst.Ino,
		binSize:       bst.Size,
		binMTimeNS:    di.BinaryMTimeNS,
		version:       ver,
		cacheRoot:     canonCache,
		cacheVolID:    rootID.Persistent.VolumeID,
		cacheDev:      cDev,
		cacheIno:      rootID.Persistent.Inode,
		toolDir:       canonToolDir,
		protectedEnvs: envs,
		stats:         stats,
		volAvail:      volAvail,
		digest:        sum,
	}, nil
}

func refuseBroadRoot(canon string) error {
	cleaned := filepath.Clean(canon)
	if cleaned == "/" || cleaned == "/Users" || cleaned == "/home" || cleaned == "/var" || cleaned == "/tmp" || cleaned == "/private/tmp" {
		return errs.Newf(errs.CodeScopeRefused, "refusing top-level directory %q as a package cache root", cleaned)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if ch, err := filepath.EvalSymlinks(home); err == nil && cleaned == filepath.Clean(ch) {
			return errs.Newf(errs.CodeScopeRefused, "refusing home directory %q as a package cache root", cleaned)
		}
	}
	return nil
}

func pathsOverlap(a, b string) bool {
	ca := filepath.Clean(a)
	cb := filepath.Clean(b)
	if ca == cb {
		return true
	}
	sep := string(filepath.Separator)
	return strings.HasPrefix(ca, cb+sep) || strings.HasPrefix(cb, ca+sep)
}

func runUVCommand(ctx context.Context, binPath, forceCacheDir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, binPath, args...)
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "UV_CACHE_DIR=") || strings.HasPrefix(kv, "UV_NO_CACHE=") || strings.HasPrefix(kv, "UV_TOOL_DIR=") {
			continue
		}
		env = append(env, kv)
	}
	if forceCacheDir != "" {
		env = append(env, "UV_CACHE_DIR="+forceCacheDir)
	}
	env = append(env, "UV_PYTHON_DOWNLOADS=never")
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	combined := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
	if err != nil {
		if combined != "" {
			return combined, fmt.Errorf("%w (%s)", err, compactOutput(combined))
		}
		return "", err
	}
	return combined, nil
}

func measureCacheRoot(cacheRoot string, expectedDev, expectedIno uint64) (treeStats, int64, error) {
	r, err := fsops.OpenRoot(cacheRoot)
	if err != nil {
		return treeStats{}, 0, errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer r.Close()
	id, _, err := r.Stat()
	if err != nil {
		return treeStats{}, 0, errs.Wrap(errs.CodeIOError, err)
	}
	if r.Dev() != expectedDev || id.Inode != expectedIno {
		return treeStats{}, 0, errs.New(errs.CodeScopeChanged, "uv cache directory identity changed")
	}

	var volAvail int64
	var stfs unix.Statfs_t
	if unix.Fstatfs(r.DirFD(), &stfs) == nil && stfs.Bsize > 0 {
		bsize := uint64(stfs.Bsize)
		if stfs.Bavail <= math.MaxInt64/bsize {
			volAvail = int64(stfs.Bavail * bsize)
		}
	}

	seen := map[uint64]struct{}{}
	queue := []string{""}
	var st treeStats

	for len(queue) > 0 {
		rel := queue[0]
		queue = queue[1:]
		var relBytes []byte
		if rel != "" {
			relBytes = []byte(rel)
		}
		d, err := r.OpenDir(relBytes)
		if err != nil {
			continue
		}
		entries, rerr := d.Readdir(-1)
		if rerr != nil {
			d.Close()
			continue
		}
		for _, e := range entries {
			var est unix.Stat_t
			if unix.Fstatat(d.DirFD(), e.Name(), &est, unix.AT_SYMLINK_NOFOLLOW) != nil {
				continue
			}
			if uint64(est.Dev) != r.Dev() {
				continue
			}
			mt := time.Unix(est.Mtim.Sec, est.Mtim.Nsec).UnixNano()
			if mt > st.latestMTimeNS {
				st.latestMTimeNS = mt
			}
			switch est.Mode & unix.S_IFMT {
			case unix.S_IFDIR:
				child := e.Name()
				if rel != "" {
					child = rel + "/" + e.Name()
				}
				queue = append(queue, child)
			case unix.S_IFLNK:
				// symlinks inside cache are not followed
			default:
				st.files++
				if _, dup := seen[est.Ino]; dup {
					continue
				}
				seen[est.Ino] = struct{}{}
				if est.Size > 0 {
					st.logicalBytes += est.Size
				}
				if est.Blocks > 0 && est.Blocks <= math.MaxInt64/512 {
					st.allocatedByte += est.Blocks * 512
				}
			}
		}
		d.Close()
	}
	return st, volAvail, nil
}

func discoverProtectedEnvs(toolDir string) []EnvTarget {
	if toolDir == "" {
		return []EnvTarget{}
	}
	entries, err := os.ReadDir(toolDir)
	if err != nil {
		return []EnvTarget{}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	out := []EnvTarget{}
	for _, e := range entries {
		p := filepath.Join(toolDir, e.Name())
		if e.Type()&os.ModeSymlink != 0 {
			if target, err := filepath.EvalSymlinks(p); err == nil {
				out = append(out, EnvTarget{Name: e.Name(), Link: p, Target: target})
			}
			continue
		}
		if e.IsDir() {
			binSub := filepath.Join(p, "bin")
			bins, berr := os.ReadDir(binSub)
			if berr != nil {
				continue
			}
			for _, b := range bins {
				bp := filepath.Join(binSub, b.Name())
				if target, err := filepath.EvalSymlinks(bp); err == nil {
					if info, serr := os.Stat(target); serr == nil && info.Mode().IsRegular() {
						out = append(out, EnvTarget{Name: e.Name() + "/" + b.Name(), Link: bp, Target: target})
					}
				}
				if len(out) >= 64 {
					return out
				}
			}
		}
		if len(out) >= 64 {
			break
		}
	}
	return out
}

func verifyProtectedEnvs(envs []EnvTarget) bool {
	for _, e := range envs {
		resolved, err := filepath.EvalSymlinks(e.Link)
		if err != nil || resolved != e.Target {
			return false
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
	}
	return true
}

func lastCompletedFor(led *ledgerFile, ins uvInspection) *ActionRecord {
	for i := len(led.Records) - 1; i >= 0; i-- {
		r := &led.Records[i]
		if r.State == "completed" && r.Tool == ToolUV && r.CacheRoot == ins.cacheRoot && r.CacheInode == ins.cacheIno {
			return r
		}
	}
	return nil
}

var uvRemovedRe = regexp.MustCompile(`(?i)removed\s+\d+\s+files?\s*\(([\d.]+)\s*([kmgtp]?i?b)\)`)

func parseUVReportedBytes(out string) *int64 {
	lower := strings.ToLower(out)
	if strings.Contains(lower, "no unused entries") || strings.Contains(lower, "nothing to remove") {
		z := int64(0)
		return &z
	}
	m := uvRemovedRe.FindStringSubmatch(out)
	if len(m) != 3 {
		return nil
	}
	val, err := strconv.ParseFloat(m[1], 64)
	if err != nil || val < 0 {
		return nil
	}
	mult := float64(1)
	switch strings.ToLower(m[2]) {
	case "b":
		mult = 1
	case "kb":
		mult = 1e3
	case "kib":
		mult = 1 << 10
	case "mb":
		mult = 1e6
	case "mib":
		mult = 1 << 20
	case "gb":
		mult = 1e9
	case "gib":
		mult = 1 << 30
	case "tb":
		mult = 1e12
	case "tib":
		mult = 1 << 40
	}
	n := int64(math.Round(val * mult))
	return &n
}

func compactOutput(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	cleaned := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			cleaned = append(cleaned, l)
		}
	}
	res := strings.Join(cleaned, " | ")
	if len(res) > 240 {
		res = res[:240]
	}
	return res
}

func loadLedger(stateDir string) (ledgerFile, error) {
	p := filepath.Join(stateDir, ledgerFileName)
	raw, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return ledgerFile{Version: ledgerVersion, Records: []ActionRecord{}}, nil
	}
	if err != nil {
		return ledgerFile{}, errs.Wrap(errs.CodeAccessDenied, err)
	}
	var lf ledgerFile
	if err := json.Unmarshal(raw, &lf); err != nil || lf.Version != ledgerVersion {
		return ledgerFile{}, errs.New(errs.CodeStateWriteFailed, "cache-actions.json is unreadable or incompatible")
	}
	return lf, nil
}

func saveLedger(stateDir string, lf ledgerFile) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	if lf.Version == 0 {
		lf.Version = ledgerVersion
	}
	if len(lf.Records) > 200 {
		// Keep all pending ops plus the most recent 200 settled entries.
		kept := make([]ActionRecord, 0, 200)
		for _, r := range lf.Records {
			if r.State == "pending" {
				kept = append(kept, r)
			}
		}
		tail := lf.Records[len(lf.Records)-180:]
		for _, r := range tail {
			if r.State != "pending" {
				kept = append(kept, r)
			}
		}
		lf.Records = kept
	}
	body, err := json.MarshalIndent(lf, "", "  ")
	if err != nil {
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	tmp, err := os.CreateTemp(stateDir, ".cache-actions-*")
	if err != nil {
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	if err := tmp.Close(); err != nil {
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	dst := filepath.Join(stateDir, ledgerFileName)
	if err := os.Rename(tmpName, dst); err != nil {
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	if pfd, err := unix.Open(stateDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0); err == nil {
		_ = unix.Fsync(pfd)
		_ = unix.Close(pfd)
	}
	return nil
}

func writeCacheReportSnapshot(stateDir string, rec ActionRecord) error {
	repDir := filepath.Join(stateDir, "cache-reports")
	if err := os.MkdirAll(repDir, 0o700); err != nil {
		return err
	}
	jBody, err := json.MarshalIndent(rec, "", "  ")
	if err == nil {
		_ = os.WriteFile(filepath.Join(repDir, "cache-"+rec.OpID+".json"), jBody, 0o600)
	}
	toolRep := "未知"
	if rec.ToolReportedBytes != nil {
		toolRep = fmt.Sprintf("%d B (%s)", *rec.ToolReportedBytes, rec.ToolReportedSummary)
	} else if rec.ToolReportedSummary != "" {
		toolRep = rec.ToolReportedSummary
	}
	md := strings.Join([]string{
		"# 工具缓存回收回执",
		"",
		fmt.Sprintf("- 操作编号：`%s`（运行 `%s`）", rec.OpID, rec.RunID),
		fmt.Sprintf("- 工具：`%s`（`%s`）", rec.Tool, rec.ToolVersion),
		fmt.Sprintf("- 缓存根目录：`%s`", rec.CacheRoot),
		fmt.Sprintf("- 实测分配块回收：`%d` B（前 `%d` B → 后 `%d` B）", rec.ReclaimedAllocatedBytes, rec.PreAllocatedBytes, rec.PostAllocatedBytes),
		fmt.Sprintf("- 实测逻辑字节变化：`%d` B（文件数 `%d` → `%d`）", rec.ReclaimedLogicalBytes, rec.PreFiles, rec.PostFiles),
		fmt.Sprintf("- 工具原始回报：%s", toolRep),
		fmt.Sprintf("- 所在卷同期可用量变化：`%+d` B", rec.VolAvailDeltaBytes),
		fmt.Sprintf("- 已安装工具环境完好：`%v`（检查项 `%d` 个）", rec.ProtectedEnvsIntact, len(rec.ProtectedEnvs)),
		"",
	}, "\n")
	return os.WriteFile(filepath.Join(repDir, "cache-"+rec.OpID+".md"), []byte(md), 0o600)
}
