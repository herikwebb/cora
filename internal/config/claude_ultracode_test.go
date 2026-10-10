package config

import (
	"strings"
	"testing"
)

func TestClaudeUltracodeConfigurationIsProviderSpecific(t *testing.T) {
	for _, test := range []struct {
		section string
		allowed bool
	}{
		{section: "reviewers.claude", allowed: true},
		{section: "escalation", allowed: true},
		{section: "reviewers.codex"},
		{section: "auto_fix"},
		{section: "reviewers.gemini"},
	} {
		t.Run(test.section, func(t *testing.T) {
			cfg, err := ApplyRepository(Defaults(), "test.toml", []byte("["+test.section+"]\neffort = \"ultracode\"\n"))
			if !test.allowed {
				if err == nil || !strings.Contains(err.Error(), test.section+".effort") {
					t.Fatalf("non-Claude ultracode must remain unsupported; got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Claude ultracode configuration rejected: %v", err)
			}
			got := cfg.Reviewers.Claude.Effort
			if test.section == "escalation" {
				got = cfg.Escalation.Effort
			}
			if got != "ultracode" {
				t.Fatalf("configured effort silently changed to %q", got)
			}
		})
	}
}

func TestInvalidEffortReportsProviderChoices(t *testing.T) {
	for _, test := range []struct {
		section string
		choices string
	}{
		{section: "reviewers.claude", choices: "low, medium, high, xhigh, max, or ultracode"},
		{section: "escalation", choices: "low, medium, high, xhigh, max, or ultracode"},
		{section: "reviewers.codex", choices: "none, minimal, low, medium, high, xhigh, max, or ultra"},
		{section: "auto_fix", choices: "none, minimal, low, medium, high, xhigh, max, or ultra"},
	} {
		t.Run(test.section, func(t *testing.T) {
			_, err := ApplyRepository(Defaults(), "test.toml", []byte("["+test.section+"]\neffort = \"maximum\"\n"))
			want := test.section + ".effort must be one of " + test.choices
			if err == nil || err.Error() != want {
				t.Fatalf("validation error = %v, want %q", err, want)
			}
		})
	}
}
