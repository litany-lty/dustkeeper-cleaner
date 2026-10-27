package recovery

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/execute"
	"dustkeeper/internal/fsops"
)

// readManifestFile reads one manifest for reporting and cross-checking. A
// manifest never authorises anything; it only helps explain a state.
func readManifestFile(sc *execute.ScopeCtx, objectID string) ([]byte, error) {
	if sc.Manifests == nil {
		return nil, errors.New("recovery: this scope has no manifest directory")
	}
	parent, err := sc.Manifests.ParentOf(objectID + ".json")
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	f, err := fsops.OpenRegularAt(parent, sc.RootDev)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, err
		}
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, errors.New("recovery: manifest is implausibly large")
	}
	return data, nil
}

func decodeB64Std(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

// quoteJSON renders a string as a JSON literal for the result payloads stored in
// the audit trail.
func quoteJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
