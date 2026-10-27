package execute

import (
	"context"

	"dustkeeper/internal/policy"
	"dustkeeper/internal/store"
)

// BuildGuard resolves protection identities with the kernel.
//
// A protection recorded as a path is only meaningful once the kernel says which
// object currently sits there. Comparing strings would miss the APFS case alias
// where "Keep/x.zip" and "keep/x.zip" are the same file.
//
// It lives here rather than in the runner because the purge path also needs it:
// a historical slot is judged by the same guard planning and quarantine use, so
// the three entry points cannot drift into three different answers.
func BuildGuard(ctx context.Context, st *store.Store, sc *ScopeCtx) (*policy.Guard, error) {
	ps, err := st.ListProtections(ctx, sc.Scope.ID, true)
	if err != nil {
		return nil, err
	}
	resolved := make([]policy.ResolvedProtection, 0, len(ps))
	for _, p := range ps {
		rp := policy.ResolvedProtection{RawRelative: p.RawRelative, MatchKind: p.MatchKind, Source: p.Source}
		if parent, perr := sc.Root.OpenParent(p.RawRelative); perr == nil {
			if id, _, serr := parent.StatAtNoFollow(); serr == nil {
				rp.Identity = id
			}
			parent.Close()
		}
		resolved = append(resolved, rp)
	}
	return policy.NewGuard(resolved), nil
}
