// Package migrations embeds the SQL baseline so that a single binary carries
// its own schema. The files live at migrations/NNN.sql, where reviewers can read
// them without opening the Go source.
package migrations

import (
	_ "embed"
	"fmt"
)

// Schema001 is the phase-one schema baseline.
//
//go:embed 001.sql
var Schema001 string

// Schema002 makes `park` a non-terminal phase and adds the purge queue cursor
// index. It is a table rebuild, because SQLite cannot alter a CHECK constraint
// in place.
//
//go:embed 002.sql
var Schema002 string

// Schema003 lets the scan queue carry "failed" and remembers which directories an
// epoch still has to prove, plus the ordering column that keeps one large
// directory from starving the rest.
//
//go:embed 003.sql
var Schema003 string

// Schema004 records review requests that were sent and produced no answer, so
// the next run does not pay for the same question twice.
//
//go:embed 004.sql
var Schema004 string

// Schema005 records a review request before it is sent, so a process that dies
// between preparing and settling a request leaves an answerable fact instead of
// a silent gap. It is additive: upgrading a 3/4 database only adds objects.
//
//go:embed 005.sql
var Schema005 string

// Schema006 records a person's reconciliation of a prepared request that never
// settled. It is a separate, append-only table: the original request row is kept
// and never disguised as settled/refused. It is additive too, so an explicit
// 3/4/5 -> 6 upgrade only creates objects.
//
//go:embed 006.sql
var Schema006 string

// Step is one forward migration.
type Step struct {
	Version int
	SQL     string
	Note    string
}

// Steps lists every forward migration in order. Applying a step is all-or-
// nothing: the caller runs the SQL and sets user_version inside one transaction.
var Steps = []Step{
	{Version: 1, SQL: Schema001, Note: "baseline"},
	{Version: 2, SQL: Schema002, Note: "park phase, purge cursor index"},
	{Version: 3, SQL: Schema003, Note: "failed scan dirs, claim order, interrupt count"},
	{Version: 4, SQL: Schema004, Note: "unresolved review requests"},
	{Version: 5, SQL: Schema005, Note: "pre-request dispatch record and settlement"},
	{Version: 6, SQL: Schema006, Note: "manual resolution of an unsettled review dispatch"},
}

// CurrentVersion is the schema version this binary writes and expects.
const CurrentVersion = 6

// OldestUpgradable is the oldest schema `init` will explicitly upgrade. A
// database older than this needs the binary that wrote it, not a leap over
// migrations whose intermediate shapes this build cannot prove.
const OldestUpgradable = 3

// For returns the steps needed to move a database from `from` to
// CurrentVersion.
func For(from int) ([]Step, error) {
	if from < 0 {
		return nil, fmt.Errorf("migrations: negative schema version %d", from)
	}
	var out []Step
	for _, s := range Steps {
		if s.Version > from && s.Version <= CurrentVersion {
			out = append(out, s)
		}
	}
	return out, nil
}
