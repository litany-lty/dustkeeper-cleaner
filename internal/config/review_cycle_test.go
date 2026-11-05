package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestReviewCycleConfigRejectsCaseAliasOverride(t *testing.T) {
	cfg := Default()
	raw, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	original := fmt.Sprintf("\"max_auto_purge_files\": %d", cfg.Runtime.MaxAutoPurgeFiles)
	withAlias := "\"max_auto_purge_files\": 0, \"MAX_AUTO_PURGE_FILES\": 100"
	body := strings.Replace(string(raw), original, withAlias, 1)
	if body == string(raw) {
		t.Fatal("fixture did not replace action budget")
	}
	if got, _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatalf("case alias silently overrides disabled purge allowance to %d", got.Runtime.MaxAutoPurgeFiles)
	}
}
