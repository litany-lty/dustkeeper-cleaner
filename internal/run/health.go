package run

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/cleanup"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/notify"
	"dustkeeper/internal/store"
)

const (
	// HealthFileName stores the latest atomic machine-readable health snapshot
	// inside the Dustkeeper state directory for external monitoring and doctor.
	HealthFileName = "health.json"
	// HealthSchemaVersion is the format version of health.json.
	HealthSchemaVersion = 1

	metaStallAlert      = "stall_alert.v1"
	stallAlertCooldown  = 24 * time.Hour
	maxHealthFileBytes  = 64 << 10
)

// HealthSnapshot summarizes whether the most recent unattended run left the
// instance healthy or stalled on an actionable condition.
type HealthSnapshot struct {
	SchemaVersion            int       `json:"schema_version"`
	UpdatedAtNS              int64     `json:"updated_at_ns"`
	LastRunID                string    `json:"last_run_id"`
	Healthy                  bool      `json:"healthy"`
	Control                  string    `json:"control"`
	FailureCode              errs.Code `json:"failure_code,omitempty"`
	ClockTrusted             bool      `json:"clock_trusted"`
	DiskPressure             bool      `json:"disk_pressure"`
	HeldScopes               []string  `json:"held_scopes,omitempty"`
	ReviewUnknownHeld        int       `json:"review_unknown_held"`
	ConsecutiveUnhealthyRuns int       `json:"consecutive_unhealthy_runs"`
	LastHealthyRunNS         int64     `json:"last_healthy_run_ns,omitempty"`
	Reasons                  []string  `json:"reasons,omitempty"`
}

type stallAlertState struct {
	Signature string `json:"signature"`
	SentAtNS  int64  `json:"sent_at_ns"`
}

func heldScopeIDs(scopes []store.Scope) []string {
	var held []string
	for _, sc := range scopes {
		if sc.Kind == domain.KindReference {
			continue
		}
		if sc.Status == domain.ScopeHeld {
			held = append(held, string(sc.ID))
		}
	}
	sort.Strings(held)
	return held
}

// EvaluateHealth derives the next HealthSnapshot from the completed run report,
// current scope rows, and any prior snapshot.
func EvaluateHealth(prev *HealthSnapshot, rep Report, scopes []store.Scope, nowNS int64) HealthSnapshot {
	held := heldScopeIDs(scopes)
	var reasons []string
	if !rep.ClockTrusted {
		reason := "clock_untrusted"
		if rep.ClockNote != "" {
			reason += ": " + rep.ClockNote
		}
		reasons = append(reasons, reason)
	}
	if len(held) > 0 {
		reasons = append(reasons, "held_scopes: "+strings.Join(held, ","))
	}
	if rep.Reconciliation.RecordFailures > 0 {
		reasons = append(reasons, "reconcile_record_failures")
	}
	if rep.ReviewUnknownHeld > 0 {
		reasons = append(reasons, fmt.Sprintf("review_unknown_held:%d", rep.ReviewUnknownHeld))
	}
	if rep.FailureCode != "" && rep.FailureCode != errs.CodeBudgetExhausted {
		reasons = append(reasons, "failure_code:"+string(rep.FailureCode))
	}

	snap := HealthSnapshot{
		SchemaVersion:     HealthSchemaVersion,
		UpdatedAtNS:       nowNS,
		LastRunID:         rep.RunID,
		Healthy:           len(reasons) == 0,
		Control:           rep.Control,
		FailureCode:       rep.FailureCode,
		ClockTrusted:      rep.ClockTrusted,
		DiskPressure:      rep.DiskPressure,
		HeldScopes:        held,
		ReviewUnknownHeld: rep.ReviewUnknownHeld,
		Reasons:           reasons,
	}
	if snap.Healthy {
		snap.ConsecutiveUnhealthyRuns = 0
		snap.LastHealthyRunNS = nowNS
		return snap
	}
	if prev != nil {
		snap.ConsecutiveUnhealthyRuns = prev.ConsecutiveUnhealthyRuns + 1
		snap.LastHealthyRunNS = prev.LastHealthyRunNS
	} else {
		snap.ConsecutiveUnhealthyRuns = 1
	}
	return snap
}

// ReadHealthFile reads and validates <stateDir>/health.json if present. It
// returns (nil, nil) when the file does not exist yet and refuses symlinks.
func ReadHealthFile(stateDir string) (*HealthSnapshot, error) {
	if stateDir == "" {
		return nil, errs.New(errs.CodeInvalidArgument, "state directory is required")
	}
	p := filepath.Join(stateDir, HealthFileName)
	info, err := os.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, errs.Wrap(errs.CodeAccessDenied, err)
	}
	if !info.Mode().IsRegular() {
		return nil, errs.New(errs.CodeStateWriteFailed, "health.json must be a regular file, not a symlink or special file")
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxHealthFileBytes+1))
	if err != nil {
		return nil, errs.Wrap(errs.CodeAccessDenied, err)
	}
	if len(data) > maxHealthFileBytes {
		return nil, errs.New(errs.CodeStateWriteFailed, "health.json exceeds 64 KiB limit")
	}
	var snap HealthSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	return &snap, nil
}

// UpdateHealthFile updates <stateDir>/health.json atomically and returns the
// written snapshot. Symlinks at the target path are refused without writing.
func UpdateHealthFile(stateDir string, rep Report, scopes []store.Scope, nowNS int64) (HealthSnapshot, error) {
	if stateDir == "" {
		return HealthSnapshot{}, errs.New(errs.CodeInvalidArgument, "state directory is required")
	}
	p := filepath.Join(stateDir, HealthFileName)
	if info, err := os.Lstat(p); err == nil && !info.Mode().IsRegular() {
		return HealthSnapshot{}, errs.New(errs.CodeStateWriteFailed, "health.json must be a regular file, not a symlink or special file")
	} else if err != nil && !os.IsNotExist(err) {
		return HealthSnapshot{}, errs.Wrap(errs.CodeAccessDenied, err)
	}

	prev, _ := ReadHealthFile(stateDir)
	snap := EvaluateHealth(prev, rep, scopes, nowNS)
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return HealthSnapshot{}, errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	raw = append(raw, '\n')

	tmp, err := os.CreateTemp(stateDir, ".health-*.tmp")
	if err != nil {
		return HealthSnapshot{}, errs.Wrap(errs.CodeIOError, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return HealthSnapshot{}, errs.Wrap(errs.CodeIOError, err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return HealthSnapshot{}, errs.Wrap(errs.CodeIOError, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return HealthSnapshot{}, errs.Wrap(errs.CodeDurabilityFailed, err)
	}
	if err := tmp.Close(); err != nil {
		return HealthSnapshot{}, errs.Wrap(errs.CodeIOError, err)
	}
	if err := os.Rename(tmpName, p); err != nil {
		return HealthSnapshot{}, errs.Wrap(errs.CodeIOError, err)
	}
	if dirFD, derr := unix.Open(stateDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0); derr == nil {
		_ = unix.Fsync(dirFD)
		_ = unix.Close(dirFD)
	}
	return snap, nil
}

func stallSignature(rep Report, scopes []store.Scope) (sig, summary string) {
	var sigParts, msgParts []string
	if !rep.ClockTrusted {
		sigParts = append(sigParts, "clock_untrusted")
		msgParts = append(msgParts, "时钟校验未通过")
	}
	if held := heldScopeIDs(scopes); len(held) > 0 {
		sigParts = append(sigParts, "held:"+strings.Join(held, ","))
		msgParts = append(msgParts, fmt.Sprintf("范围 %s 已挂起(held)", strings.Join(held, ", ")))
	}
	if rep.ReviewUnknownHeld > 0 {
		sigParts = append(sigParts, fmt.Sprintf("unknown_held:%d", rep.ReviewUnknownHeld))
		msgParts = append(msgParts, fmt.Sprintf("%d 项审核结果未知挂起", rep.ReviewUnknownHeld))
	}
	switch rep.FailureCode {
	case errs.CodeScopeChanged, errs.CodeHeld, errs.CodeRecoveryRequired, errs.CodeStateWriteFailed, errs.CodeModelUnavailable:
		sigParts = append(sigParts, "code:"+string(rep.FailureCode))
		msgParts = append(msgParts, "错误码 "+string(rep.FailureCode))
	}
	if len(sigParts) == 0 {
		return "", ""
	}
	return strings.Join(sigParts, "|"), strings.Join(msgParts, "；")
}

// maybeNotifyStall emits at most one notification per 24h for the same
// actionable unattended stall condition, clears stale dedupe state on recovery,
// and stays silent on healthy 0-action runs or when notifications already failed.
func (r *Runner) maybeNotifyStall(ctx context.Context, pol cleanup.Policy, rep *Report, scopes []store.Scope, nowNS int64) {
	if r.Notify == nil || !pol.Confirmed() || pol.Notify == cleanup.NotifyNone {
		return
	}
	for _, s := range rep.Scopes {
		if s.Notification != nil && s.Notification.Attempted && !s.Notification.Delivered {
			return
		}
		if strings.HasPrefix(s.PurgeSkipped, "notify_failed:") {
			return
		}
	}

	sig, summary := stallSignature(*rep, scopes)
	prevRaw, err := r.Store.MetaGet(ctx, metaStallAlert)
	if sig == "" {
		if err == nil && prevRaw != "" {
			_ = r.Store.MetaSet(ctx, metaStallAlert, "")
		}
		return
	}
	if err == nil && prevRaw != "" {
		var prev stallAlertState
		if json.Unmarshal([]byte(prevRaw), &prev) == nil &&
			prev.Signature == sig && prev.SentAtNS > 0 && nowNS-prev.SentAtNS < int64(stallAlertCooldown) {
			return
		}
	}

	body := fmt.Sprintf("Dustkeeper 自动清理已暂停或受阻（%s），请运行 dustkeeper doctor 查看详情。", summary)
	res := r.deliver(ctx, body)
	_ = notify.Record(ctx, r.Store, nowNS, body, res)
	if res.Delivered {
		stateJSON, merr := json.Marshal(stallAlertState{Signature: sig, SentAtNS: nowNS})
		if merr == nil {
			_ = r.Store.MetaSet(ctx, metaStallAlert, string(stateJSON))
		}
	}
}
