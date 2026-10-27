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
}

// CurrentVersion is the schema version this binary writes and expects.
const CurrentVersion = 3

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
