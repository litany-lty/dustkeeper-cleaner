// Package rescue reads a management directory without opening the database.
//
// The recovery contract says an operator must be able to explain where their
// bytes went using only what is inside the scope. R13 found that `recover
// inspect` opened SQLite first, so a damaged database - the one case where the
// independent record matters - made the vault unreadable.
//
// Nothing here writes. It opens no lock, creates no file, repairs nothing, and
// imports nothing back into an authorisation. It is an inventory, and every entry
// it cannot explain is listed rather than resolved.
package rescue

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/fsops"
)

// Entry statuses. They describe what the directory contains, not what should
// happen next.
const (
	// StatusComplete means a manifest and its object are both present.
	StatusComplete = "manifest_and_object"
	// StatusMissingObject means the manifest survived but the object did not.
	StatusMissingObject = "manifest_without_object"
	// StatusOrphanObject means an object has no manifest. It is never deleted or
	// imported: without a manifest there is no record of which file it was.
	StatusOrphanObject = "orphan_object"
	// StatusConflict means the manifest contradicts itself or its own name.
	StatusConflict = "conflict"
	// StatusUnknownPayload means an entry in the objects directory that is not a
	// plain object file.
	StatusUnknownPayload = "unknown_payload"
	// StatusUnverified means the objects directory could not be read, so this
	// inventory cannot say whether the object is there. It is deliberately not
	// "missing": an access error proves only that the check did not happen.
	StatusUnverified = "unverified"
)

// maxManifestBytes bounds one manifest read. A manifest is a small JSON object.
const maxManifestBytes = 256 << 10

// Entry is one object as the directory describes it.
type Entry struct {
	Object         string   `json:"object,omitempty"`
	Status         string   `json:"status"`
	RawRelative    string   `json:"raw_relative,omitempty"`
	RawRelativeB64 string   `json:"raw_relative_b64,omitempty"`
	Manifest       string   `json:"manifest,omitempty"`
	ContentHash    string   `json:"content_hash,omitempty"`
	MetadataHash   string   `json:"metadata_hash,omitempty"`
	ExpectedInode  uint64   `json:"expected_inode,omitempty"`
	ExpectedDevice uint64   `json:"expected_device,omitempty"`
	ObjectSize     int64    `json:"object_size_bytes,omitempty"`
	Notes          []string `json:"notes,omitempty"`
}

// Report is the inventory of one management directory.
type Report struct {
	Root       string `json:"root"`
	InstanceID string `json:"owner_instance,omitempty"`
	ScopeID    string `json:"owner_scope,omitempty"`
	Manifests  int    `json:"manifests"`
	Objects    int    `json:"objects"`
	Complete   int    `json:"complete"`
	Missing    int    `json:"manifest_without_object"`
	Orphans    int    `json:"orphan_objects"`
	Unverified int    `json:"unverified"`
	// ObjectState is read, partial, missing or unreadable. Only read supports
	// any claim that a particular object is not there.
	ObjectState string `json:"object_state"`
	// ManifestState says the same for the manifests directory. A partial listing
	// there cannot support "this object has no manifest": the manifest may be in
	// the part that was not listed.
	ManifestState string   `json:"manifest_state"`
	Conflicts     int      `json:"conflicts"`
	Unknown       int      `json:"unknown_payloads"`
	Entries       []Entry  `json:"entries"`
	Notes         []string `json:"notes,omitempty"`
}

// Inspect inventories the private directory inside rootPath.
//
// The directory itself is opened through fsops, so the same rule applies as
// everywhere else: no symlink is followed and no component is re-resolved. A
// missing management directory is reported as "not a managed scope", not as an
// empty vault.
func Inspect(rootPath string) (Report, error) {
	rep := Report{Root: rootPath}

	root, err := fsops.OpenRoot(rootPath)
	if err != nil {
		return rep, errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer root.Close()

	vault, err := root.OpenDir([]byte(domain.VaultDirName))
	if err != nil {
		return rep, errs.Newf(errs.CodeScopeRefused,
			"%q has no %s directory, so it is not a managed scope", rootPath, domain.VaultDirName)
	}
	defer vault.Close()

	if id, scopeID, note := readOwner(vault); note != "" {
		rep.Notes = append(rep.Notes, note)
	} else {
		rep.InstanceID, rep.ScopeID = id, scopeID
	}

	manifests, manifestState, notes := readManifests(root)
	rep.Notes = append(rep.Notes, notes...)
	rep.Manifests = len(manifests)
	rep.ManifestState = manifestState

	inv := readObjects(root)
	rep.Notes = append(rep.Notes, inv.notes...)
	objects, unknown := inv.entries, inv.unknown
	rep.Objects = len(objects)
	rep.Unknown = len(unknown)
	rep.ObjectState = inv.state
	rep.Notes = append(rep.Notes, inv.conclusion())

	for _, name := range unknown {
		rep.Entries = append(rep.Entries, Entry{
			Object: name, Status: StatusUnknownPayload,
			Notes: []string{"not a plain object file; left untouched"},
		})
	}

	// Manifests first, in object-name order, then orphans, so a reader can follow
	// the same order every time.
	for _, m := range manifests {
		e := Entry{
			Object:         m.manifest.ObjectID,
			Manifest:       m.name,
			RawRelativeB64: m.manifest.RawRelativeB64,
			ContentHash:    m.manifest.ContentHash,
			MetadataHash:   m.manifest.MetadataHash,
			ExpectedInode:  m.manifest.ExpectedIdentity.Inode,
			ExpectedDevice: m.manifest.ExpectedIdentity.Device,
		}
		if raw, err := base64.StdEncoding.DecodeString(m.manifest.RawRelativeB64); err == nil {
			e.RawRelative = string(raw)
		}
		if m.problem != "" {
			e.Status = StatusConflict
			e.Notes = append(e.Notes, m.problem)
			rep.Conflicts++
		}
		if inv.state == objectStateUnreadable {
			if e.Status == "" {
				e.Status = StatusUnverified
				rep.Unverified++
			}
		} else if obj, ok := objects[m.manifest.ObjectID]; ok {
			e.ObjectSize = obj.size
			delete(objects, m.manifest.ObjectID)
			if e.Status == "" {
				if obj.err != "" {
					// The entry could not be examined, which is not the same fact as
					// "the object is not there" and not a contradiction either.
					e.Status = StatusUnverified
					e.Notes = append(e.Notes, "the object entry could not be examined: "+obj.err)
					rep.Unverified++
				} else {
					e.Status = StatusComplete
					rep.Complete++
				}
			}
		} else if e.Status == "" {
			if inv.state == objectStatePartial {
				// The listing stopped early. This object is not in the part that was
				// read, which is not evidence that it is absent.
				e.Status = StatusUnverified
				e.Notes = append(e.Notes,
					"the objects directory could not be read to the end; this object may be in the part that was not listed")
				rep.Unverified++
			} else {
				// Reached only when the directory was read in full, or when it is
				// known to be entirely absent. Both establish that this object is
				// not there.
				e.Status = StatusMissingObject
				e.Notes = append(e.Notes, "the manifest is intact but the object is gone; this is not a recoverable payload")
				rep.Missing++
			}
		}
		rep.Entries = append(rep.Entries, e)
	}

	if inv.state == objectStateUnreadable {
		return rep, nil
	}
	orphanNames := make([]string, 0, len(objects))
	for name := range objects {
		orphanNames = append(orphanNames, name)
	}
	sort.Strings(orphanNames)
	for _, name := range orphanNames {
		o := objects[name]
		e := Entry{Object: name, Status: StatusOrphanObject, ObjectSize: o.size}
		if rep.ManifestState == ManifestStatePartial {
			// "No manifest" is a statement about the manifests directory, and that
			// directory did not finish. The object is real; the conclusion is not.
			e.Status = StatusUnverified
			e.Notes = append(e.Notes,
				"the manifests directory could not be read to the end; this object may have a manifest that was not listed")
			rep.Unverified++
			rep.Entries = append(rep.Entries, e)
			continue
		}
		e.Notes = append(e.Notes,
			"no manifest: the original path cannot be reconstructed from this directory alone")
		if o.err != "" {
			e.Notes = append(e.Notes, "the entry could not be read: "+o.err)
		}
		rep.Orphans++
		rep.Entries = append(rep.Entries, e)
	}
	return rep, nil
}

// readOwner reads the marker for display. It is not a safety decision here: an
// unreadable marker is reported, and the inventory continues.
func readOwner(vault *fsops.Dir) (instanceID, scopeID, note string) {
	body, err := vault.ReadFileAt("owner.json", maxManifestBytes)
	if err != nil {
		return "", "", fmt.Sprintf("the owner marker could not be read: %v", err)
	}
	var m struct {
		InstanceID string `json:"instance_id"`
		ScopeID    string `json:"scope_id"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return "", "", fmt.Sprintf("the owner marker is not readable JSON: %v", err)
	}
	return m.InstanceID, m.ScopeID, ""
}

type manifestEntry struct {
	name     string
	manifest execute.ManifestV1
	problem  string
}

// States of a directory listing. "I saw these names" and "I saw all of them" are
// different facts, and only the second one supports a negative conclusion.
const (
	listStateRead    = "read"
	listStatePartial = "partial"
	listStateMissing = "missing"
)

// Exported listing states for the report, so callers do not match on strings.
const (
	ManifestStateRead    = listStateRead
	ManifestStatePartial = listStatePartial
	ManifestStateMissing = listStateMissing
)

// dirListing is one directory read: what it gave, and whether it gave all of it.
type dirListing struct {
	names []string
	state string
	notes []string
}

// readManifests reads every manifest without opening anything for writing.
func readManifests(root *fsops.Root) ([]manifestEntry, string, []string) {
	dir, err := root.OpenDir([]byte(domain.VaultManifests))
	if err != nil {
		return nil, listStateMissing, []string{fmt.Sprintf("the manifests directory is missing: %v", err)}
	}
	defer dir.Close()

	listing := listDir(dir)
	state := listing.state
	notes := listing.notes
	var out []manifestEntry
	for _, name := range listing.names {
		if !strings.HasSuffix(name, ".json") {
			notes = append(notes, "ignored a non-manifest entry in the manifests directory: "+name)
			continue
		}
		body, err := dir.ReadFileAt(name, maxManifestBytes)
		if err != nil {
			out = append(out, manifestEntry{
				name: name, problem: fmt.Sprintf("the manifest could not be read: %v", err),
			})
			continue
		}
		var m execute.ManifestV1
		if err := json.Unmarshal(body, &m); err != nil {
			out = append(out, manifestEntry{
				name: name, problem: fmt.Sprintf("the manifest is not readable JSON: %v", err),
			})
			continue
		}
		stem := strings.TrimSuffix(name, ".json")
		if m.ObjectID != stem {
			out = append(out, manifestEntry{name: name, manifest: m,
				problem: fmt.Sprintf("the manifest names object %q but the file is %q", m.ObjectID, stem)})
			continue
		}
		if m.FormatVersion != 1 {
			out = append(out, manifestEntry{name: name, manifest: m,
				problem: fmt.Sprintf("unsupported manifest format version %d", m.FormatVersion)})
			continue
		}
		if m.ContentHash == "" || m.MetadataHash == "" {
			out = append(out, manifestEntry{name: name, manifest: m,
				problem: "the manifest records no content or metadata hash, so its object cannot be identified"})
			continue
		}
		out = append(out, manifestEntry{name: name, manifest: m})
	}
	sortManifestEntries(out)
	return out, state, notes
}

type objectEntry struct {
	size int64
	err  string
}

// The four states an objects directory can be in. Only objectStateRead supports
// a statement about a particular object being absent; objectStateMissing
// establishes that the whole directory is gone, which also establishes that no
// object is in it; objectStatePartial saw some names and no more; and
// objectStateUnreadable establishes nothing at all.
const (
	objectStateRead       = "read"
	objectStatePartial    = "partial"
	objectStateMissing    = "missing"
	objectStateUnreadable = "unreadable"
)

// Exported names for the report's object_state field, so a caller does not have
// to match a string to know which fact it is holding.
const (
	ObjectStateRead       = objectStateRead
	ObjectStatePartial    = objectStatePartial
	ObjectStateMissing    = objectStateMissing
	ObjectStateUnreadable = objectStateUnreadable
)

type objectInventory struct {
	entries map[string]objectEntry
	unknown []string
	notes   []string
	state   string
}

// readObjects lists the objects directory. It never opens a payload for reading:
// the inventory only needs to know what is there.
func readObjects(root *fsops.Root) objectInventory {
	inv := objectInventory{entries: map[string]objectEntry{}}
	dir, err := root.OpenDir([]byte(domain.VaultObjects))
	if err != nil {
		// A missing directory and an unreadable one are different facts. Saying
		// "the object is gone" because permission was denied would turn an
		// unavailable check into a conclusion about the data.
		if errors.Is(err, unix.ENOENT) {
			inv.state = objectStateMissing
			inv.notes = []string{"the objects directory does not exist"}
			return inv
		}
		inv.state = objectStateUnreadable
		inv.notes = []string{fmt.Sprintf("the objects directory cannot be opened: %v", err)}
		return inv
	}
	defer dir.Close()

	listing := listDir(dir)
	inv.notes = append(inv.notes, listing.notes...)
	// A listing that stopped halfway is neither "read" nor "unreadable": the
	// names it gave are real, and the ones it never reached are unknown. Saying
	// "read" here is what turns an unfinished inventory into "the object is
	// gone" for every manifest after the point where reading stopped.
	inv.state = listing.state

	out := inv.entries
	var unknown []string
	for _, name := range listing.names {
		if name == domain.VaultNonceName {
			// The vault's own creation evidence, not a payload.
			continue
		}
		id, tok, err := dir.StatEntry(name)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				inv.notes = append(inv.notes, "an object disappeared while it was being listed: "+name)
				continue
			}
			out[name] = objectEntry{err: err.Error()}
			continue
		}
		if id.Kind != "regular" {
			unknown = append(unknown, name)
			continue
		}
		out[name] = objectEntry{size: tok.Size}
	}
	inv.unknown = unknown
	return inv
}

// conclusion states what this inventory is allowed to claim about the objects it
// did not see. The three cases are different facts, so they get different
// sentences rather than one hedge covering all of them.
func (inv objectInventory) conclusion() string {
	switch inv.state {
	case objectStateRead:
		return "a manifest_and_object entry means the two names exist; the inventory compares no hash and no identity"
	case objectStateMissing:
		return "the objects directory does not exist, so every manifest is reported as having no object"
	case objectStatePartial:
		return "the objects directory could not be read to the end: objects that were not listed are unknown, not absent"
	default:
		return "the objects directory could not be read: absence of an object is not established"
	}
}

// listDir returns the entry names of a directory, sorted, and says whether it
// reached the end.
//
// A read error is reported rather than swallowed: an inventory that silently
// stops halfway is worse than one that says it could not finish, because the
// operator would conclude the rest of the vault is empty. The caller decides what
// an incomplete listing permits; this function only refuses to hide it.
func listDir(d *fsops.Dir) dirListing {
	var out dirListing
	for {
		if err := listFault(d.Rel(), len(out.names)); err != nil {
			// The listing was interrupted before the directory confirmed it had
			// given everything. What was collected is kept; what it permits is
			// decided by the caller.
			out.state = listStatePartial
			out.notes = []string{"the directory could not be read to the end: " + err.Error()}
			break
		}
		batch, err := d.Readdir(128)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
				break
			}
			out.state = listStatePartial
			out.notes = []string{"the directory could not be read to the end: " + err.Error()}
			break
		}
		if len(batch) == 0 {
			break
		}
		for _, e := range batch {
			out.names = append(out.names, e.Name())
		}
	}
	if out.state == "" {
		out.state = listStateRead
	}
	sort.Strings(out.names)
	return out
}

// sortManifestEntries orders by object name so that two inventories of the same
// directory read the same way.
func sortManifestEntries(s []manifestEntry) {
	sort.SliceStable(s, func(i, j int) bool {
		return s[i].manifest.ObjectID < s[j].manifest.ObjectID
	})
}

// ParseRootArg validates the --root value. It refuses a relative path and a path
// that is not a directory, so a typo cannot be reported as "empty vault".
func ParseRootArg(p string) (string, error) {
	if p == "" {
		return "", errs.New(errs.CodeInvalidArgument, "--root is required")
	}
	clean := path.Clean(p)
	if !path.IsAbs(clean) {
		return "", errs.Newf(errs.CodeInvalidArgument, "--root must be an absolute path, got %q", p)
	}
	return clean, nil
}
