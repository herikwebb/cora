package gitx

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestParseNameStatusPathsIncludesBothSidesOfRenamesAndCopies(t *testing.T) {
	raw := []byte("M\x00ordinary.go\x00R100\x00auth/token.go\x00docs/token.go\x00C075\x00source.go\x00copy.go\x00")
	paths, err := parseNameStatusPaths(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ordinary.go", "auth/token.go", "docs/token.go", "source.go", "copy.go"}
	if !slices.Equal(paths, want) {
		t.Fatalf("parsed paths = %v, want %v", paths, want)
	}
}

func TestParseNameStatusPathsRejectsTruncatedRename(t *testing.T) {
	if _, err := parseNameStatusPaths([]byte("R100\x00old.go\x00")); err == nil {
		t.Fatal("expected truncated rename to fail")
	}
}

func TestChangedPathsIncludesSensitiveRenameSourceAndDestination(t *testing.T) {
	ctx := context.Background()
	root := newGitRepository(t)
	if err := os.MkdirAll(filepath.Join(root, "auth"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeGitFile(t, filepath.Join(root, "auth", "token.go"), "package auth\n")
	gitTest(t, root, "add", "auth/token.go")
	gitTest(t, root, "commit", "-m", "feat(auth): add token validation")
	gitTest(t, root, "switch", "-c", "rename-auth")
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitTest(t, root, "mv", "auth/token.go", "docs/token.go")
	gitTest(t, root, "commit", "-m", "refactor(auth): relocate token validation")

	repo, err := Discover(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	target, err := repo.ResolveTarget(ctx, TargetOptions{Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := repo.ChangedPaths(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"auth/token.go", "docs/token.go"} {
		if !slices.Contains(paths, path) {
			t.Fatalf("renamed path %q missing from %v", path, paths)
		}
	}
}
