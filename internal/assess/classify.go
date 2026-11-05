package assess

import (
	"path/filepath"
	"strings"
)

// Categories. The strings are the values the advisory report has always used;
// they are named here so that the cleanup policy can validate a requested set
// instead of accepting free text.
const (
	// CatUserRecord covers project sources, build records and non-document
	// assets. A name in this class is a reason to ask, never a reason to delete.
	CatUserRecord = "potential_user_record"
	// CatCredential covers keys, wallets and credential exports. Never a
	// candidate, whatever its timestamps say.
	CatCredential = "potential_credential_or_recovery_data"
	// CatDocument covers documents, tables, presentations and backup-looking
	// names. Reviewable, but its disposal needs a basis that a name cannot give.
	CatDocument = "document_or_backup_candidate"
	// CatInstaller covers .dmg/.pkg/.exe-style installers. Re-obtainable in
	// principle; whether this copy is disposable is still a separate question.
	CatInstaller = "installer_candidate"
	// CatArchive covers archives whose contents are unknown.
	CatArchive = "archive_unknown_contents"
	// CatTemporary covers names that only look temporary (.tmp/.cache/...).
	CatTemporary = "temporary_name_only"
	// CatDeclaredTemp is a path inside a directory the operator declared
	// temporary. The declaration is recorded, not treated as authority.
	CatDeclaredTemp = "declared_temporary_origin"
	// CatStale is the fallthrough: old or unknown, with nothing else to say.
	CatStale = "stale_or_unknown"
)

// KnownCategories lists every category this build can produce, in a stable
// order. Policy validation uses it so that a typo is rejected rather than
// silently matching nothing.
func KnownCategories() []string {
	return []string{CatUserRecord, CatCredential, CatDocument, CatInstaller, CatArchive, CatTemporary, CatDeclaredTemp, CatStale}
}

// Classification is what a file name alone can establish.
//
// It deliberately carries no timestamp and no content: the whole point of
// separating the name from the evidence is that "looks like an installer" and
// "has not been touched in 40 days" are two weak signals, and neither may be
// presented as the reason a file is disposable.
type Classification struct {
	Category string
	Reason   string
	// KeepName reports that the class itself requires retention (project
	// sources, credentials). The caller must not offer it to a model.
	KeepName bool
	// ImportantName reports a name that reads like a document or a backup
	// ("合同", "invoice", ...). It adds a reason, never an exemption.
	ImportantName bool
}

// ClassifyName classifies a file by its base name.
func ClassifyName(base string) Classification {
	name := strings.ToLower(base)
	ext := strings.ToLower(filepath.Ext(name))

	if isProjectName(name) {
		return Classification{Category: CatUserRecord, Reason: "project_source_or_build_record", KeepName: true}
	}
	if isCredentialName(name, ext) {
		return Classification{Category: CatCredential, Reason: "credential_or_recovery_signal", KeepName: true}
	}
	if strings.Contains(name, "private_key") || strings.Contains(name, "recovery") {
		return Classification{Category: CatCredential, Reason: "credential_or_recovery_signal", KeepName: true}
	}
	if isAssetExtension(ext) {
		return Classification{Category: CatUserRecord, Reason: "non_document_asset_or_credential_type", KeepName: true}
	}
	important := false
	for _, v := range []string{"备份", "合同", "账单", "发票", "backup", "invoice"} {
		if strings.Contains(name, v) {
			important = true
			break
		}
	}
	switch {
	case important || isDocumentExtension(ext):
		return Classification{Category: CatDocument, Reason: "document_type_is_not_a_permanent_exemption", ImportantName: important}
	case isInstallerExtension(ext):
		return Classification{Category: CatInstaller, Reason: "installer_copy_may_be_reobtainable"}
	case isArchiveExtension(ext):
		return Classification{Category: CatArchive, Reason: "archive_contents_unknown"}
	case isTemporaryExtension(ext):
		return Classification{Category: CatTemporary, Reason: "temporary_name_only"}
	default:
		return Classification{Category: CatStale, Reason: "stale_or_unknown"}
	}
}

func isProjectName(name string) bool {
	switch name {
	case "makefile", "dockerfile", "cmakelists.txt", "cargo.lock", "go.mod", "go.sum", "gemfile":
		return true
	}
	return false
}

func isCredentialName(name, ext string) bool {
	switch name {
	case ".env", "id_rsa", "id_ed25519", "id_ecdsa", "wallet.dat", ".npmrc", ".pypirc", "accesskey.csv", "osskey.txt":
		return true
	}
	if strings.HasPrefix(name, ".env.") {
		return true
	}
	switch ext {
	case ".p12", ".pfx", ".gpg", ".kdbx":
		return true
	}
	return false
}

// isAssetExtension is the class that made the first real inventory report
// useless when it was treated as stale: fonts, source, media and databases are
// records, not garbage.
func isAssetExtension(ext string) bool {
	switch ext {
	case ".toml", ".ini", ".conf", ".plist", ".properties", ".gradle",
		".m4a", ".wav", ".mp3", ".flac", ".aac", ".ogg", ".mbox", ".eml", ".pst", ".sqlite3",
		".sh", ".bash", ".zsh", ".fish", ".c", ".h", ".cpp", ".hpp", ".rs", ".swift", ".kt", ".rb", ".php", ".vue", ".svelte", ".jsx", ".tsx", ".key", ".pem", ".jpg", ".jpeg", ".png", ".heic", ".mov", ".mp4", ".sqlite", ".db", ".py", ".go", ".java", ".js", ".ts", ".cs", ".json", ".yaml", ".yml", ".ttf", ".otf", ".woff", ".woff2":
		return true
	}
	return false
}

func isDocumentExtension(ext string) bool {
	switch ext {
	case ".xml", ".bak", ".old", ".orig", ".pages", ".numbers", ".docm", ".xlsm", ".pptm", ".sql", ".pdf", ".doc", ".docx", ".xls", ".xlsx", ".csv", ".md", ".txt", ".rtf", ".ppt", ".pptx", ".odt", ".ods", ".odp", ".epub", ".mobi", ".ics":
		return true
	}
	return false
}

func isInstallerExtension(ext string) bool {
	switch ext {
	case ".dmg", ".pkg", ".msi", ".exe", ".appimage", ".deb", ".rpm", ".apk":
		return true
	}
	return false
}

func isArchiveExtension(ext string) bool {
	switch ext {
	case ".zip", ".tar", ".gz", ".7z", ".tgz", ".tbz2", ".xz", ".rar", ".zst":
		return true
	}
	return false
}

func isTemporaryExtension(ext string) bool {
	switch ext {
	case ".tmp", ".temp", ".cache", ".part", ".crdownload", ".download":
		return true
	}
	return false
}
