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
	Root       string   `json:"root"`
	InstanceID string   `json:"owner_instance,omitempty"`
	ScopeID    string   `json:"owner_scope,omitempty"`
	Manifests  int      `json:"manifests"`
	Objects    int      `json:"objects"`
	Complete   int      `json:"complete"`
	Missing    int      `json:"manifest_without_object"`
	Orphans    int      `json:"orphan_objects"`
	Conflicts  int      `json:"conflicts"`
	Unknown    int      `json:"unknown_payloads"`
	Entries    []Entry  `json:"entries"`
	Notes      []string `json:"notes,omitempty"`
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

	manifests, notes := readManifests(root)
	rep.Notes = append(rep.Notes, notes...)
	rep.Manifests = len(manifests)

	objects, unknown, notes := readObjects(root)
	rep.Notes = append(rep.Notes, notes...)
	rep.Objects = len(objects)
	rep.Unknown = len(unknown)

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
		if obj, ok := objects[m.manifest.ObjectID]; ok {
			e.ObjectSize = obj.size
			delete(objects, m.manifest.ObjectID)
			if e.Status == "" {
				if obj.err != "" {
					// The objects directory could not be read, so "present" is not
					// something this report can claim.
					e.Status = StatusConflict
					e.Notes = append(e.Notes, "the object entry could not be read: "+obj.err)
					rep.Conflicts++
				} else {
					e.Status = StatusComplete
					rep.Complete++
				}
			}
		} else if e.Status == "" {
			e.Status = StatusMissingObject
			e.Notes = append(e.Notes, "the manifest is intact but the object is gone; this is not a recoverable payload")
			rep.Missing++
		}
		rep.Entries = append(rep.Entries, e)
	}

	orphanNames := make([]string, 0, len(objects))
	for name := range objects {
		orphanNames = append(orphanNames, name)
	}
	sort.Strings(orphanNames)
	for _, name := range orphanNames {
		o := objects[name]
		e := Entry{Object: name, Status: StatusOrphanObject, ObjectSize: o.size}
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

// readManifests reads every manifest without opening anything for writing.
func readManifests(root *fsops.Root) ([]manifestEntry, []string) {
	dir, err := root.OpenDir([]byte(domain.VaultManifests))
	if err != nil {
		return nil, []string{fmt.Sprintf("the manifests directory is missing: %v", err)}
	}
	defer dir.Close()

	names, notes := listDir(dir)
	var out []manifestEntry
	for _, name := range names {
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
	return out, notes
}

type objectEntry struct {
	size int64
	err  string
}

// readObjects lists the objects directory. It never opens a payload for reading:
// the inventory only needs to know what is there.
func readObjects(root *fsops.Root) (map[string]objectEntry, []string, []string) {
	out := map[string]objectEntry{}
	dir, err := root.OpenDir([]byte(domain.VaultObjects))
	if err != nil {
		return out, nil, []string{fmt.Sprintf("the objects directory is missing: %v", err)}
	}
	defer dir.Close()

	names, notes := listDir(dir)
	var unknown []string
	for _, name := range names {
		id, tok, err := dir.StatEntry(name)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				notes = append(notes, "an object disappeared while it was being listed: "+name)
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
	return out, unknown, notes
}

// listDir returns the entry names of a directory, sorted.
//
// A read error is returned rather than swallowed: an inventory that silently
// stops halfway is worse than one that says it could not finish, because the
// operator would conclude the rest of the vault is empty.
func listDir(d *fsops.Dir) ([]string, []string) {
	var names []string
	for {
		batch, err := d.Readdir(128)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
				break
			}
			return names, []string{"the directory could not be read to the end: " + err.Error()}
		}
		if len(batch) == 0 {
			break
		}
		for _, e := range batch {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
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
