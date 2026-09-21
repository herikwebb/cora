//go:build !windows

package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/herikwebb/cora/internal/config"
	"github.com/herikwebb/cora/internal/gitx"
)

func TestSecurityAnalysisIncludesRenameSourceAndDestination(t *testing.T) {
	ctx := context.Background()
	root := orchestratorTestRepo(t)
	if err := os.MkdirAll(filepath.Join(root, "auth"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "auth", "token.go"), []byte("package auth\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", "auth/token.go")
	gitRun(t, root, "commit", "-m", "feat(auth): add token handling")
	gitRun(t, root, "switch", "-c", "move-token")
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "mv", "auth/token.go", "docs/token.go")
	gitRun(t, root, "commit", "-m", "docs: move token example")

	repo, err := gitx.Discover(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	target, err := repo.ResolveTarget(ctx, gitx.TargetOptions{Base: "main", RequireClean: true})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := repo.ChangedPaths(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(paths, "auth/token.go") || !slices.Contains(paths, "docs/token.go") {
		t.Fatalf("rename paths = %v, want both source and destination", paths)
	}
	_, sensitive := AnalyzeSecurityPaths(paths, config.Defaults().Escalation.SecurityPathMarkers)
	if !slices.Contains(sensitive, "auth/token.go") {
		t.Fatalf("security-sensitive rename source was lost: paths=%v sensitive=%v", paths, sensitive)
	}
}
