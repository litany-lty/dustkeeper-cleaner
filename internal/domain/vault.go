package domain

// The private management directory and its fixed contents.
//
// The names live in one place because two packages must agree on them exactly:
// internal/scope creates and validates the directory while internal/execute
// opens it on every run. When the two disagreed, the executor could open an
// object that registration never verified.
const (
	// VaultDirName is the private management directory inside a mutable scope.
	// observe and reference scopes never get one.
	VaultDirName = ".dustkeeper"
	// VaultObjects holds quarantined payloads, named by item ID.
	VaultObjects = VaultDirName + "/objects"
	// VaultManifests holds the independent recovery fact for each object.
	VaultManifests = VaultDirName + "/manifests"
	// VaultTmp is the only place a partial private write is staged.
	VaultTmp = VaultDirName + "/tmp"
	// VaultAnchor is the same-volume file a full device barrier is issued on.
	VaultAnchor = VaultDirName + "/durability.anchor"
	// VaultMarker records which instance owns the directory.
	VaultMarker = VaultDirName + "/owner.json"
	// VaultScopeLock is the in-tree mutex that spans state directories. It lives
	// inside the management directory so that a second installation cannot act on
	// the same tree even though its own instance lock is free.
	VaultScopeLock = "scope.lock"
)
