package provider

import (
	"reflect"
	"testing"

	"github.com/herikwebb/cora/internal/config"
)

func TestCodexUltraPersistsDelegationParentWithoutChangingIsolation(t *testing.T) {
	tests := []struct {
		name string
		args func(string) []string
	}{
		{
			name: "review",
			args: func(effort string) []string {
				return codexReviewArgs(config.Reviewer{Model: "gpt-6-astra", Effort: effort}, Request{
					WorkDir: "/tmp/workspace", RuntimeDir: "/tmp/runtime", RecoveryDir: "/tmp/recovery",
					SchemaPath: "/tmp/schema.json", Policy: "trusted policy",
				}, "/tmp/result.json")
			},
		},
		{
			name: "auto-fix",
			args: func(effort string) []string {
				return codexFixArgs(config.AutoFix{Model: "gpt-6-astra", Effort: effort}, FixRequest{
					RepoRoot: "/tmp/workspace", Policy: "trusted policy",
				}, "/tmp/result.json")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			high := test.args("high")
			var want []string
			ephemeralCount := 0
			for _, arg := range high {
				if arg == "--ephemeral" {
					ephemeralCount++
					continue
				}
				if arg == `model_reasoning_effort="high"` {
					arg = `model_reasoning_effort="ultra"`
				}
				want = append(want, arg)
			}
			if ephemeralCount != 1 {
				t.Fatalf("ordinary invocation must remain ephemeral: %v", high)
			}
			got := test.args("ultra")
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ultra changed flags beyond effort and persistence:\ngot  %v\nwant %v", got, want)
			}
			for _, flag := range []string{"--ignore-rules", "--ignore-user-config", "--skip-git-repo-check"} {
				found := false
				for _, arg := range got {
					found = found || arg == flag
				}
				if !found {
					t.Fatalf("isolation flag %s missing: %v", flag, got)
				}
			}
			for i, arg := range got {
				if arg == "--sandbox" && (i+1 == len(got) || got[i+1] != "workspace-write") {
					t.Fatalf("sandbox changed: %v", got)
				}
			}
		})
	}
}
