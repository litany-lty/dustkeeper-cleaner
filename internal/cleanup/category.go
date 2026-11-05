package cleanup

import (
	"path/filepath"
	"strings"

	"dustkeeper/internal/assess"
)

// CategoryOf answers "what kind of file is this" from its relative path alone.
//
// It is a pure function of the path, so the planner, the executor and the
// result page all reach the same answer without sharing a cache or a database
// column. Only the class comes from here; whether the file may actually be
// deleted is decided per item by DirectDelete/disposition.
func CategoryOf(relPath string) string {
	return assess.ClassifyName(filepath.Base(filepath.Clean(relPath))).Category
}

// CategoryInside answers the same question for a path below a declared
// temporary directory.
//
// The declaration is recorded rather than trusted: a file whose name says
// nothing but which lives under a directory the operator declared temporary is
// still only "declared temporary", not "safe to delete". A classifier that let
// a directory declaration outrank the file's own class would turn one mistake
// at setup time into a whole subtree of deletions.
func CategoryInside(relPath string, declaredTempPrefixes []string) string {
	cat := CategoryOf(relPath)
	if cat != assess.CatStale && cat != assess.CatTemporary {
		return cat
	}
	clean := filepath.ToSlash(filepath.Clean(relPath))
	for _, prefix := range declaredTempPrefixes {
		p := strings.Trim(filepath.ToSlash(filepath.Clean(prefix)), "/")
		if p == "" || p == "." {
			continue
		}
		if clean == p || strings.HasPrefix(clean, p+"/") {
			return assess.CatDeclaredTemp
		}
	}
	return cat
}

// MayDeleteDirectly reports whether the confirmed policy lets this category be
// removed without an observation period.
func (p Policy) MayDeleteDirectly(category string) bool {
	if category == assess.CatCredential || category == assess.CatUserRecord {
		return false
	}
	return p.DirectDelete(category)
}
