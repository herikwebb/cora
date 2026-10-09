package config

import (
	"strings"
	"testing"
)

func TestCodexUltraConfigurationIsProviderSpecific(t *testing.T) {
	for _, section := range []string{"reviewers.codex", "auto_fix", "reviewers.claude", "escalation"} {
		t.Run(section, func(t *testing.T) {
			cfg, err := ApplyRepository(Defaults(), "test.toml", []byte("["+section+"]\neffort = \"ultra\"\n"))
			if section == "reviewers.claude" || section == "escalation" {
				if err == nil || !strings.Contains(err.Error(), section+".effort") {
					t.Fatalf("Claude ultra must remain unsupported; got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Codex ultra configuration rejected: %v", err)
			}
			got := cfg.Reviewers.Codex.Effort
			if section == "auto_fix" {
				got = cfg.AutoFix.Effort
			}
			if got != "ultra" {
				t.Fatalf("configured effort silently changed to %q", got)
			}
		})
	}
}
