// Package dedup finds exact duplicates.
//
// "Duplicate" in phase one means: same size, same full SHA-256, identical bytes
// under a full comparison, and compatible metadata. A hash match alone is a
// candidate, never a conclusion, and content equality never by itself authorises
// removing a path.
package dedup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/fsops"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/scope"
	"dustkeeper/internal/store"
)

// Candidate is one indexed file that may be a duplicate.
type Candidate struct {
	ScopeID domain.ScopeID
	Rel     []byte
	// Parent is the directory that held the file when it was indexed. The
	// executor re-checks it just before moving anything: a path whose parent
	// was replaced is a different file's path, not this one's.
	Parent     domain.FileIdentity
	ParentJSON string
	Identity   domain.FileIdentity
	Token      domain.ChangeToken
	Hash       domain.Digest
	Meta       fsops.MetadataEvidence
	MetaHash   domain.Digest
}

// Group is one set of files with identical bytes.
type Group struct {
	Size   int64
	Hash   domain.Digest
	Keeper Candidate
	Others []Candidate
	// MetadataCompatible is false when the copies differ in xattrs, mode,
	// ownership or mtime; phase one then keeps them all.
	MetadataCompatible bool
	Note               string
}

// Detector performs the duplicate pipeline.
type Detector struct {
	Store      *store.Store
	Registrar  *scope.Registrar
	Clock      domain.Clock
	Buffer     []byte
	Budget     *domain.Budget
	RatePerSec int64
	MaxBytes   int64
}

// Result summarises a detection pass.
type Result struct {
	ContentGroups   int      `json:"content_groups"`
	PlannableGroups int      `json:"plannable_groups"`
	Candidates      int      `json:"candidates"`
	Selected        int      `json:"selected"`
	LogicalBytes    int64    `json:"logical_bytes"`
	Groups          []Group  `json:"groups"`
	Notes           []string `json:"notes"`
	Truncated       bool     `json:"truncated"`
}

// Detect runs size grouping, full hashing, byte comparison and metadata checks.
//
// The keeper scope, when given, is searched first: a file already living in a
// reference scope is a better keeper than one inside the disposable scope.
func (d *Detector) Detect(ctx context.Context, source *execute.ScopeCtx, keeperScopeID *domain.ScopeID) (Result, error) {
	var res Result

	// The reference scope is opened once. A copy that already lives there is a
	// better keeper than one inside the scope being cleaned, because it is the
	// copy the user explicitly described as the original.
	var keeperScope *execute.ScopeCtx
	if keeperScopeID != nil && *keeperScopeID != "" && *keeperScopeID != source.Scope.ID {
		ks, err := execute.OpenScopeCtx(ctx, d.Registrar, *keeperScopeID)
		if err != nil {
			return res, err
		}
		defer ks.Close()
		keeperScope = ks
	}

	// 1. Admissible candidates only: plain regular files with a single link.
	bySize := map[int64][]Candidate{}
	err := d.Store.IterateObservations(ctx, source.Scope.ID, func(o store.Observation) error {
		if o.Status != domain.ObsSeen || o.SizeBytes <= 0 {
			return nil
		}
		id, err := decodeIdentity(o.IdentityJSON)
		if err != nil {
			return nil
		}
		tok, err := decodeToken(o.TokenJSON)
		if err != nil {
			return nil
		}
		if tok.NLink != 1 || id.Kind != "regular" {
			return nil
		}
		parent, perr := decodeIdentity(o.ParentJSON)
		if perr != nil {
			return nil
		}
		bySize[o.SizeBytes] = append(bySize[o.SizeBytes], Candidate{
			ScopeID: source.Scope.ID, Rel: o.RawRelative,
			Parent: parent, ParentJSON: o.ParentJSON,
			Identity: id, Token: tok,
		})
		return nil
	})
	if err != nil {
		return res, err
	}

	// 2. Hash every file in a size group that has more than one member.
	byHash := map[string][]Candidate{}
	for size, group := range bySize {
		if len(group) < 2 {
			continue
		}
		res.ContentGroups++
		for _, c := range group {
			if d.Budget != nil && (d.Budget.TimeUp(d.Clock) || d.Budget.ReadUp()) {
				res.Truncated = true
				res.Notes = append(res.Notes, "the run budget was reached before every group was hashed")
				break
			}
			got, err := d.hashCandidate(source, c)
			if err != nil {
				res.Notes = append(res.Notes, string(c.Rel)+": "+err.Error())
				continue
			}
			c.Hash = got
			key := string(got)
			byHash[key] = append(byHash[key], c)
		}
		_ = size
	}

	// 3. Byte comparison and metadata equivalence against the keeper.
	keys := make([]string, 0, len(byHash))
	for k := range byHash {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		members := byHash[key]
		if len(members) < 2 {
			continue
		}
		sort.Slice(members, func(i, j int) bool {
			if members[i].Token.MTimeNS != members[j].Token.MTimeNS {
				return members[i].Token.MTimeNS < members[j].Token.MTimeNS
			}
			return string(members[i].Rel) < string(members[j].Rel)
		})

		g := Group{Size: members[0].Token.Size, Hash: domain.Digest(key), Keeper: members[0]}
		keeperFile, keeperCleanup, err := openCandidate(source, g.Keeper)
		if err != nil {
			res.Notes = append(res.Notes, "keeper unreadable: "+err.Error())
			continue
		}
		equal := true
		for _, m := range members[1:] {
			other, cleanup, err := openCandidate(source, m)
			if err != nil {
				res.Notes = append(res.Notes, string(m.Rel)+": "+err.Error())
				continue
			}
			same, err := execute.CompareFiles(keeperFile, other, execute.VerifyOptions{
				Buffer: d.Buffer, Budget: d.Budget, RatePerSec: d.RatePerSec,
			})
			cleanup()
			if err != nil || !same {
				equal = false
				res.Notes = append(res.Notes,
					"content differs despite an equal hash: "+string(m.Rel))
				continue
			}
			g.Others = append(g.Others, m)
		}
		keeperCleanup()

		if !equal || len(g.Others) == 0 {
			continue
		}

		if keeperScope != nil {
			d.preferReferenceKeeper(ctx, source, keeperScope, &g, &res)
			if len(g.Others) == 0 {
				continue
			}
		}

		g.MetadataCompatible, g.Note = d.metadataCompatible(source, keeperScope, g)
		res.Groups = append(res.Groups, g)
		if !g.MetadataCompatible {
			continue
		}
		res.PlannableGroups++
		for range g.Others {
			res.Candidates++
		}
	}
	return res, nil
}

// hashCandidate opens the file, proves it is still the indexed object, and
// returns its full SHA-256.
func (d *Detector) hashCandidate(sc *execute.ScopeCtx, c Candidate) (domain.Digest, error) {
	f, cleanup, err := openCandidate(sc, c)
	if err != nil {
		return "", err
	}
	defer cleanup()
	h, _, err := execute.HashFile(f, execute.VerifyOptions{
		Buffer: d.Buffer, Budget: d.Budget, RatePerSec: d.RatePerSec,
	})
	return h, err
}

func openCandidate(sc *execute.ScopeCtx, c Candidate) (*fsops.File, func(), error) {
	parent, err := sc.Root.OpenParent(c.Rel)
	if err != nil {
		return nil, func() {}, errs.Wrap(errs.CodeAccessDenied, err)
	}
	f, err := fsops.OpenRegularAt(parent, sc.RootDev)
	if err != nil {
		parent.Close()
		return nil, func() {}, errs.Wrap(errs.CodeAccessDenied, err)
	}
	if !f.Identity.Equal(c.Identity) {
		f.Close()
		parent.Close()
		return nil, func() {}, errs.New(errs.CodePlanStale, "the file was replaced since the scan")
	}
	return f, func() { f.Close(); parent.Close() }, nil
}

// metadataCompatible checks that the copies agree on the metadata that carries
// meaning. Disagreement keeps every copy: two files can be byte-identical and
// still not be interchangeable.
func (d *Detector) metadataCompatible(sc, keeperScope *execute.ScopeCtx, g Group) (bool, string) {
	keeperFile, cleanup, err := openCandidate(scopeFor(g.Keeper, sc, keeperScope), g.Keeper)
	if err != nil {
		return false, "keeper unreadable"
	}
	kMeta, err := execute.MetadataOf(keeperFile)
	cleanup()
	if err != nil || kMeta.Unsupported != "" {
		return false, "keeper metadata cannot be normalised"
	}
	keeperDigest := execute.MetadataDigest(kMeta, g.Keeper.Token)

	for _, o := range g.Others {
		f, cleanup, err := openCandidate(scopeFor(o, sc, keeperScope), o)
		if err != nil {
			return false, "a copy is unreadable"
		}
		meta, err := execute.MetadataOf(f)
		cleanup()
		if err != nil || meta.Unsupported != "" {
			return false, "a copy's metadata cannot be normalised"
		}
		if execute.MetadataDigest(meta, o.Token) != keeperDigest {
			return false, "the copies differ in mtime, permissions, ownership or extended attributes"
		}
	}
	return true, ""
}

// scopeFor picks the scope context a candidate belongs to.
func scopeFor(c Candidate, source, keeper *execute.ScopeCtx) *execute.ScopeCtx {
	if keeper != nil && c.ScopeID == keeper.Scope.ID {
		return keeper
	}
	return source
}

// preferReferenceKeeper replaces the in-scope keeper with an identical file from
// the reference scope.
//
// If the reference scope holds no identical copy, the group is unchanged: a
// keeper is never invented, and the copies in the scope being cleaned are never
// all removed.
func (d *Detector) preferReferenceKeeper(ctx context.Context, source, keeper *execute.ScopeCtx,
	g *Group, res *Result) {

	var candidates []Candidate
	err := d.Store.IterateObservations(ctx, keeper.Scope.ID, func(o store.Observation) error {
		if o.Status != domain.ObsSeen || o.SizeBytes != g.Size {
			return nil
		}
		id, ierr := decodeIdentity(o.IdentityJSON)
		if ierr != nil {
			return nil
		}
		tok, terr := decodeToken(o.TokenJSON)
		if terr != nil {
			return nil
		}
		if tok.NLink != 1 || id.Kind != "regular" {
			return nil
		}
		parent, perr := decodeIdentity(o.ParentJSON)
		if perr != nil {
			return nil
		}
		candidates = append(candidates, Candidate{
			ScopeID: keeper.Scope.ID, Rel: o.RawRelative,
			Parent: parent, ParentJSON: o.ParentJSON,
			Identity: id, Token: tok,
		})
		return nil
	})
	if err != nil {
		res.Notes = append(res.Notes, "the reference scope could not be read: "+err.Error())
		return
	}
	sort.Slice(candidates, func(i, j int) bool { return string(candidates[i].Rel) < string(candidates[j].Rel) })

	for _, c := range candidates {
		if d.Budget != nil && (d.Budget.TimeUp(d.Clock) || d.Budget.ReadUp()) {
			res.Truncated = true
			res.Notes = append(res.Notes, "the run budget was reached before the reference scope was searched")
			return
		}
		got, herr := d.hashCandidate(keeper, c)
		if herr != nil || got != g.Hash {
			continue
		}
		// Byte equality was already proven for the in-scope keeper against the
		// group hash, so an equal hash here proves it against every copy too.
		old := g.Keeper
		c.Hash = got
		g.Keeper = c
		g.Others = append(g.Others, old)
		return
	}
}

// VerifyKeeper re-proves that a duplicate's surviving copy is intact.
//
// It runs immediately before a purge. If the keeper is missing, replaced,
// modified or unreadable, the purge is blocked: choosing another copy on the
// spot would risk deleting the last one.
func (d *Detector) VerifyKeeper(ctx context.Context, it store.Item) error {
	if it.KeeperJSON == nil {
		return errs.New(errs.CodeHeld, "this duplicate has no recorded keeper")
	}
	var ev plan.KeeperEvidenceV1
	if err := json.Unmarshal([]byte(*it.KeeperJSON), &ev); err != nil {
		return errs.Newf(errs.CodeHeld, "the keeper record is unreadable: %v", err)
	}
	rel, err := plan.UnB64(ev.RawRelativeB64)
	if err != nil {
		return errs.New(errs.CodeHeld, "the keeper path is unreadable")
	}

	sc, err := execute.OpenScopeCtx(ctx, d.Registrar, domain.ScopeID(ev.ScopeID))
	if err != nil {
		return errs.New(errs.CodeHeld, "the keeper scope cannot be verified")
	}
	defer sc.Close()

	parent, err := sc.Root.OpenParent(rel)
	if err != nil {
		return errs.Newf(errs.CodeHeld, "the keeper %q is missing", rel)
	}
	defer parent.Close()
	f, err := fsops.OpenRegularAt(parent, sc.RootDev)
	if err != nil {
		return errs.Newf(errs.CodeHeld, "the keeper %q cannot be opened", rel)
	}
	defer f.Close()

	if !f.Identity.Equal(ev.Identity) {
		return errs.New(errs.CodeHeld, "the keeper is no longer the recorded object")
	}
	if f.Token.Size != ev.Token.Size || f.Token.MTimeNS != ev.Token.MTimeNS {
		return errs.New(errs.CodeHeld, "the keeper was modified after the plan was made")
	}

	opt := execute.VerifyOptions{Buffer: d.Buffer, RatePerSec: d.RatePerSec, Budget: d.Budget}
	hash, _, err := execute.HashFile(f, opt)
	if err != nil {
		return err
	}
	if string(hash) != ev.ContentHash {
		return errs.New(errs.CodeHeld, "the keeper's content no longer matches the plan")
	}
	if it.ContentHash != nil && *it.ContentHash != ev.ContentHash {
		return errs.New(errs.CodeHeld, "the keeper does not hold the same content as this copy")
	}
	return nil
}

// SizeOf returns the file size of a candidate; used by callers that cap a plan
// by logical bytes.
func SizeOf(c Candidate) int64 { return c.Token.Size }

func decodeIdentity(s string) (domain.FileIdentity, error) {
	var id domain.FileIdentity
	err := json.Unmarshal([]byte(s), &id)
	return id, err
}

func decodeToken(s string) (domain.ChangeToken, error) {
	var t domain.ChangeToken
	err := json.Unmarshal([]byte(s), &t)
	return t, err
}

// errNotFound is used by tests that assert absence rather than an I/O failure.
var errNotFound = errors.New("not found")

// statIsAbsent reports whether an error means "the name is not there". Any other
// error must not be read as absence.
func statIsAbsent(err error) bool {
	return errors.Is(err, unix.ENOENT) || errors.Is(err, os.ErrNotExist)
}
