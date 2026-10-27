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
	//
	// A group with exactly one in-scope member still has to be hashed when a
	// reference scope is being searched: one copy here and one copy there is a
	// duplicate, and the old code dropped that case before the reference query
	// even ran.
	byHash := map[string][]Candidate{}
	for size, group := range bySize {
		if len(group) < 2 && keeperScope == nil {
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
		// One member is enough only when a reference scope might hold the other
		// copy; without a keeper scope a lone file is not a duplicate.
		if len(members) < 2 && keeperScope == nil {
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

		if !equal {
			continue
		}

		// The reference scope is consulted before the "is there anything to do"
		// question is answered, because in the one-to-one case the answer only
		// becomes yes after the swap.
		if keeperScope != nil {
			d.preferReferenceKeeper(ctx, source, keeperScope, &g, &res)
		}
		if len(g.Others) == 0 {
			continue
		}

		g.MetadataCompatible, g.Note = d.metadataCompatible(source, keeperScope, &g)
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
func (d *Detector) metadataCompatible(sc, keeperScope *execute.ScopeCtx, g *Group) (bool, string) {
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
	// The digest is recorded on the candidate so it reaches the keeper evidence in
	// the plan. R09 found this field declared, read by create.go, and never
	// assigned - so every keeper carried an empty metadata hash and the purge
	// check had nothing to compare against.
	g.Keeper.Meta = kMeta
	g.Keeper.MetaHash = keeperDigest

	for i := range g.Others {
		o := &g.Others[i]
		f, cleanup, err := openCandidate(scopeFor(*o, sc, keeperScope), *o)
		if err != nil {
			return false, "a copy is unreadable"
		}
		meta, err := execute.MetadataOf(f)
		cleanup()
		if err != nil || meta.Unsupported != "" {
			return false, "a copy's metadata cannot be normalised"
		}
		digest := execute.MetadataDigest(meta, o.Token)
		if digest != keeperDigest {
			return false, "the copies differ in mtime, permissions, ownership or extended attributes"
		}
		o.Meta = meta
		o.MetaHash = digest
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
		// An equal hash is a claim, not proof. The replacement keeper has to hold
		// the same bytes and the same extended metadata as the copy it replaces,
		// because the old keeper becomes a deletion candidate after the swap.
		if !d.referenceKeeperMatches(source, keeper, c, *g) {
			continue
		}
		old := g.Keeper
		c.Hash = got
		g.Keeper = c
		// A swap invalidates the metadata digest recorded for the old keeper.
		g.Keeper.MetaHash = ""
		g.Keeper.Meta = fsops.MetadataEvidence{}
		g.Others = append(g.Others, old)
		return
	}
}

// referenceKeeperMatches proves a reference-scope candidate really is
// interchangeable with the group's current keeper.
func (d *Detector) referenceKeeperMatches(source, keeper *execute.ScopeCtx, c Candidate, g Group) bool {
	opt := execute.VerifyOptions{Buffer: d.Buffer, Budget: d.Budget, RatePerSec: d.RatePerSec}

	cand, candCleanup, err := openCandidate(keeper, c)
	if err != nil {
		return false
	}
	defer candCleanup()
	cur, curCleanup, err := openCandidate(source, g.Keeper)
	if err != nil {
		return false
	}
	defer curCleanup()

	same, err := execute.CompareFiles(cur, cand, opt)
	if err != nil || !same {
		return false
	}
	curMeta, err := execute.MetadataOf(cur)
	if err != nil || curMeta.Unsupported != "" {
		return false
	}
	candMeta, err := execute.MetadataOf(cand)
	if err != nil || candMeta.Unsupported != "" {
		return false
	}
	return execute.MetadataDigest(candMeta, c.Token) == execute.MetadataDigest(curMeta, g.Keeper.Token)
}

// VerifyKeeper re-proves that a duplicate's surviving copy is intact.
//
// It runs immediately before a purge. If the keeper is missing, replaced,
// modified or unreadable, the purge is blocked: choosing another copy on the
// spot would risk deleting the last one.
//
// R09 lists what this check used to skip: the extended metadata, the link count,
// the state after the read finished, and any byte comparison against the copy
// about to be deleted. Each of those is now required. The bytes matter most: an
// equal hash is a claim, and comparing the two files is the evidence.
//
// The keeper is left open in the returned proof. The byte comparison above is
// the last full read of either file, so the caller has to be able to re-read the
// keeper's identity, token and metadata after it: R09 A records that nothing did,
// and a write that lands in a region the comparison has already passed still
// leaves the two files equal for that comparison.
func (d *Detector) VerifyKeeper(ctx context.Context, it store.Item) (execute.KeeperProof, error) {
	if it.KeeperJSON == nil {
		return nil, errs.New(errs.CodeHeld, "this duplicate has no recorded keeper")
	}
	var ev plan.KeeperEvidenceV1
	if err := json.Unmarshal([]byte(*it.KeeperJSON), &ev); err != nil {
		return nil, errs.Newf(errs.CodeHeld, "the keeper record is unreadable: %v", err)
	}
	rel, err := plan.UnB64(ev.RawRelativeB64)
	if err != nil {
		return nil, errs.New(errs.CodeHeld, "the keeper path is unreadable")
	}
	if ev.MetadataHash == "" {
		// A plan made without metadata evidence cannot prove equivalence now.
		// Refusing is the only honest answer; guessing that "no evidence" means
		// "no difference" is how a non-equivalent copy gets deleted.
		return nil, errs.New(errs.CodeHeld,
			"the plan recorded no metadata evidence for this keeper, so equivalence cannot be re-proven")
	}

	opt := execute.VerifyOptions{Buffer: d.Buffer, RatePerSec: d.RatePerSec, Budget: d.Budget}

	sc, err := execute.OpenScopeCtx(ctx, d.Registrar, domain.ScopeID(ev.ScopeID))
	if err != nil {
		return nil, errs.New(errs.CodeHeld, "the keeper scope cannot be verified")
	}
	parent, err := sc.Root.OpenParent(rel)
	if err != nil {
		sc.Close()
		return nil, errs.Newf(errs.CodeHeld, "the keeper %q is missing", rel)
	}
	f, err := fsops.OpenRegularAt(parent, sc.RootDev)
	parent.Close()
	// The descriptor stays valid on its own; only the scope lock is released, so
	// the proof does not hold a scope while the purge continues.
	sc.Close()
	if err != nil {
		return nil, errs.Newf(errs.CodeHeld, "the keeper %q cannot be opened", rel)
	}
	proven := false
	defer func() {
		if !proven {
			f.Close()
		}
	}()

	if !f.Identity.Equal(ev.Identity) {
		return nil, errs.New(errs.CodeHeld, "the keeper is no longer the recorded object")
	}

	// One stable read establishes content, metadata and the post-read identity in
	// the same shape the payload path uses.
	ev2, err := execute.ReadStable(f, opt)
	if err != nil {
		if errors.Is(err, execute.ErrUnstableObject) {
			return nil, errs.Newf(errs.CodeHeld, "the keeper changed while it was being verified: %v", err)
		}
		return nil, err
	}
	if ev2.Token.NLink != 1 {
		return nil, errs.Newf(errs.CodeHeld,
			"the keeper now has %d links; another name can outlive the copy being deleted",
			ev2.Token.NLink)
	}
	if ev2.Token.Size != ev.Token.Size || ev2.Token.MTimeNS != ev.Token.MTimeNS {
		return nil, errs.New(errs.CodeHeld, "the keeper was modified after the plan was made")
	}
	if ev2.Token.Mode != ev.Token.Mode || ev2.Token.UID != ev.Token.UID || ev2.Token.GID != ev.Token.GID {
		return nil, errs.New(errs.CodeHeld,
			"the keeper's permissions or ownership changed after the plan was made")
	}
	if string(ev2.Content) != ev.ContentHash {
		return nil, errs.New(errs.CodeHeld, "the keeper's content no longer matches the plan")
	}
	if string(ev2.Metadata) != ev.MetadataHash {
		return nil, errs.New(errs.CodeHeld, "the keeper's extended metadata no longer matches the plan")
	}
	if it.ContentHash != nil && *it.ContentHash != ev.ContentHash {
		return nil, errs.New(errs.CodeHeld, "the keeper does not hold the same content as this copy")
	}

	// Finally, compare the two files byte for byte. The quarantined copy is still
	// in the vault, so the comparison is against the bytes that are about to be
	// deleted, not against a remembered digest.
	obj, closeObj, err := openQuarantined(ctx, d.Registrar, it)
	if err != nil {
		return nil, errs.Newf(errs.CodeHeld, "the quarantined copy cannot be read for comparison: %v", err)
	}
	defer closeObj()
	same, err := execute.CompareFiles(f, obj, opt)
	if err != nil {
		return nil, err
	}
	if !same {
		return nil, errs.New(errs.CodeHeld,
			"the keeper and the quarantined copy hold different bytes despite an equal hash")
	}

	proven = true
	return &keeperProof{file: f, want: ev, metadata: ev2.Metadata}, nil
}

// keeperProof carries the verified keeper and its open descriptor to the last
// check boundary before the unlink.
type keeperProof struct {
	file     *fsops.File
	want     plan.KeeperEvidenceV1
	metadata domain.Digest
}

// Confirm re-reads the keeper through the descriptor that was verified.
//
// It compares identity, link count, size, mtime, permissions, ownership and the
// extended-metadata digest. Content is not read again: the point is to detect a
// change that landed after the comparison had passed the region, not to claim a
// snapshot that a concurrent writer cannot break.
func (p *keeperProof) Confirm(ctx context.Context) error {
	if p == nil || p.file == nil {
		return errs.New(errs.CodeHeld, "the keeper was not held open; its state cannot be re-read")
	}
	if err := ctx.Err(); err != nil {
		return errs.Wrap(errs.CodeCancelled, err)
	}
	id, tok, err := p.file.Stat()
	if err != nil {
		return errs.Newf(errs.CodeHeld, "the keeper can no longer be inspected: %v", err)
	}
	if !id.Equal(p.want.Identity) {
		return errs.New(errs.CodeHeld, "the keeper is no longer the recorded object")
	}
	if tok.NLink != 1 {
		return errs.Newf(errs.CodeHeld,
			"the keeper now has %d links; another name can outlive the copy being deleted", tok.NLink)
	}
	if tok.Size != p.want.Token.Size || tok.MTimeNS != p.want.Token.MTimeNS {
		return errs.New(errs.CodeHeld, "the keeper changed after the final comparison")
	}
	if tok.Mode != p.want.Token.Mode || tok.UID != p.want.Token.UID || tok.GID != p.want.Token.GID {
		return errs.New(errs.CodeHeld,
			"the keeper's permissions or ownership changed after the final comparison")
	}
	meta, err := execute.MetadataOf(p.file)
	if err != nil {
		return err
	}
	if meta.Unsupported != "" {
		return errs.Newf(errs.CodeHeld,
			"the keeper's extended metadata can no longer be established (%s); refusing to treat it as equivalent",
			meta.Unsupported)
	}
	if got := execute.MetadataDigest(meta, tok); got != p.metadata {
		return errs.New(errs.CodeHeld, "the keeper's extended metadata changed after the final comparison")
	}
	return nil
}

// Close releases the keeper descriptor.
func (p *keeperProof) Close() error {
	if p == nil || p.file == nil {
		return nil
	}
	err := p.file.Close()
	p.file = nil
	return err
}

// openQuarantined opens one object inside a scope's private vault.
func openQuarantined(ctx context.Context, reg *scope.Registrar, it store.Item) (*fsops.File, func(), error) {
	sc, err := execute.OpenScopeCtx(ctx, reg, it.ScopeID)
	if err != nil {
		return nil, func() {}, err
	}
	parent, err := sc.Objects.ParentOf(it.ObjectName)
	if err != nil {
		sc.Close()
		return nil, func() {}, err
	}
	f, err := fsops.OpenRegularAt(parent, sc.RootDev)
	if err != nil {
		parent.Close()
		sc.Close()
		return nil, func() {}, err
	}
	return f, func() { f.Close(); parent.Close(); sc.Close() }, nil
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
