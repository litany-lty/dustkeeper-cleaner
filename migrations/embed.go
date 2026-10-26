// Package migrations embeds the SQL baseline so that a single binary carries
// its own schema. The file itself lives at migrations/001.sql, where reviewers
// can read it without opening the Go source.
package migrations

import _ "embed"

// Schema001 is the phase-one schema baseline.
//
//go:embed 001.sql
var Schema001 string

// CurrentVersion is the schema version this binary writes and expects.
const CurrentVersion = 1
