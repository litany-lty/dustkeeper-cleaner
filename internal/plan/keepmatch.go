package plan

import (
	"context"
	"encoding/json"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/outcome"
	"dustkeeper/internal/store"
)

// KeepEntry is one valid, non-error keep decision extracted from an audit event.
type KeepEntry struct {
	ScopeID    string
	RawRel     string
	RuleDigest string
	Action     string
	Identity   domain.FileIdentity
	Token      domain.ChangeToken
}

// KeepIndex indexes recent valid keep decisions for one scope so both planning
// and execution share a single matching implementation without nested queries.
type KeepIndex struct {
	byRel map[string][]KeepEntry
}

// LoadKeepIndex loads every valid keep decision for scopeID at or after sinceNS.
func LoadKeepIndex(ctx context.Context, st *store.Store, scopeID domain.ScopeID, sinceNS int64) (*KeepIndex, error) {
	events, err := st.KeepEventsForScopeSince(ctx, string(scopeID), sinceNS)
	if err != nil {
		return nil, err
	}
	idx := &KeepIndex{byRel: make(map[string][]KeepEntry)}
	for _, ev := range events {
		entry, ok := parseKeepEntry(ev, string(scopeID))
		if !ok {
			continue
		}
		idx.byRel[entry.RawRel] = append(idx.byRel[entry.RawRel], entry)
	}
	return idx, nil
}

func parseKeepEntry(ev store.Event, scopeID string) (KeepEntry, bool) {
	if ev.ResultJSON == nil {
		return KeepEntry{}, false
	}
	var contract struct {
		ScopeID     string `json:"scope_id"`
		RawRelative string `json:"raw_relative_b64"`
		RuleDigest  string `json:"rule_digest"`
		Action      string `json:"action"`
	}
	if ev.ContractJSON != "" {
		if err := json.Unmarshal([]byte(ev.ContractJSON), &contract); err != nil {
			return KeepEntry{}, false
		}
	}
	if contract.ScopeID != "" && scopeID != "" && contract.ScopeID != scopeID {
		return KeepEntry{}, false
	}
	raw, err := UnB64(contract.RawRelative)
	if err != nil {
		return KeepEntry{}, false
	}
	var payload struct {
		Identity    string `json:"identity"`
		Token       string `json:"token"`
		Reason      string `json:"reason"`
		ModelError  string `json:"model_error"`
		Explanation *struct {
			Cause string `json:"cause"`
		} `json:"review_explanation"`
	}
	if err := json.Unmarshal([]byte(*ev.ResultJSON), &payload); err != nil {
		return KeepEntry{}, false
	}
	// Never treat an outage, budget stop, unresolved hold or transient local
	// fact refusal as a verdict about the file.
	if payload.ModelError != "" {
		return KeepEntry{}, false
	}
	switch payload.Reason {
	case outcome.ReasonReviewUnavailable,
		outcome.ReasonNotStartedBudget,
		outcome.ReasonReviewUnknown,
		outcome.ReasonRequestCheckHold,
		outcome.ReasonFactsMissing:
		return KeepEntry{}, false
	}
	if payload.Explanation != nil {
		switch payload.Explanation.Cause {
		case "local_facts_missing", "local_move_safety_unknown":
			return KeepEntry{}, false
		}
	}
	if payload.Identity == "" || payload.Token == "" {
		return KeepEntry{}, false
	}
	var keptID domain.FileIdentity
	var keptTok domain.ChangeToken
	if json.Unmarshal([]byte(payload.Identity), &keptID) != nil ||
		json.Unmarshal([]byte(payload.Token), &keptTok) != nil {
		return KeepEntry{}, false
	}
	return KeepEntry{
		ScopeID:    contract.ScopeID,
		RawRel:     string(raw),
		RuleDigest: contract.RuleDigest,
		Action:     contract.Action,
		Identity:   keptID,
		Token:      keptTok,
	}, true
}

// Kept reports whether this exact object version already received a valid keep
// decision for the given action and rule digest.
func (k *KeepIndex) Kept(rel, action, ruleDigest string, id domain.FileIdentity, tok domain.ChangeToken) bool {
	if k == nil {
		return false
	}
	for _, entry := range k.byRel[rel] {
		if !keepActionMatches(entry.Action, action) {
			continue
		}
		if action != domain.ActionPurge && entry.RuleDigest != "" && ruleDigest != "" && entry.RuleDigest != ruleDigest {
			continue
		}
		if SameStableObject(id, tok, entry.Identity, entry.Token) {
			return true
		}
	}
	return false
}

// keepActionMatches keeps purge decisions strictly separate from pre-isolation
// decisions. Legacy keep events recorded before the action field existed were
// written only on the quarantine/direct-delete path.
func keepActionMatches(recorded, want string) bool {
	if want == domain.ActionPurge {
		return recorded == domain.ActionPurge
	}
	return recorded != domain.ActionPurge
}

// SameStableObject compares persistent object identity and change token fields,
// matching the executor's stability rule.
func SameStableObject(now domain.FileIdentity, tok domain.ChangeToken, wantID domain.FileIdentity, wantTok domain.ChangeToken) bool {
	if !now.Equal(wantID) {
		return false
	}
	if now.Kind != "regular" {
		return false
	}
	if tok.Size != wantTok.Size || tok.MTimeNS != wantTok.MTimeNS || tok.NLink != wantTok.NLink ||
		tok.Mode != wantTok.Mode || tok.UID != wantTok.UID || tok.GID != wantTok.GID {
		return false
	}
	if wantTok.CTimeNS != 0 && tok.CTimeNS != wantTok.CTimeNS {
		return false
	}
	return true
}

type pendingKey struct {
	action   string
	identity string
	token    string
}

type unknownKey struct {
	rel      string
	action   string
	identity string
	token    string
}

// PendingIndex holds unresolved review_unknown and pending review_dispatch rows
// for one scope so planning can skip frozen objects without nested DB queries.
type PendingIndex struct {
	unknowns   map[unknownKey]bool
	dispatches map[pendingKey]bool
}

// LoadPendingIndex loads all unresolved review requests for scopeID.
func LoadPendingIndex(ctx context.Context, st *store.Store, scopeID domain.ScopeID) (*PendingIndex, error) {
	unknowns, err := st.OpenReviewUnknownForScope(ctx, string(scopeID))
	if err != nil {
		return nil, err
	}
	dispatches, err := st.OpenDispatchesForScope(ctx, string(scopeID))
	if err != nil {
		return nil, err
	}
	idx := &PendingIndex{
		unknowns:   make(map[unknownKey]bool, len(unknowns)),
		dispatches: make(map[pendingKey]bool, len(dispatches)),
	}
	for _, u := range unknowns {
		raw, derr := UnB64(u.PathB64)
		if derr != nil {
			continue
		}
		idx.unknowns[unknownKey{
			rel:      string(raw),
			action:   u.Action,
			identity: u.IdentityJSON,
			token:    u.TokenJSON,
		}] = true
	}
	for _, d := range dispatches {
		idx.dispatches[pendingKey{
			action:   d.Action,
			identity: d.IdentityJSON,
			token:    d.TokenJSON,
		}] = true
	}
	return idx, nil
}

// Held reports whether an unresolved request exists for this object and action.
func (p *PendingIndex) Held(rel, action, identityJSON, tokenJSON string) bool {
	if p == nil {
		return false
	}
	if p.unknowns[unknownKey{rel: rel, action: action, identity: identityJSON, token: tokenJSON}] {
		return true
	}
	return p.dispatches[pendingKey{action: action, identity: identityJSON, token: tokenJSON}]
}
