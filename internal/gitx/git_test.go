package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestResolveTargetsAndVerifyFingerprint(t *testing.T) {
	ctx := context.Background()
	root := newGitRepository(t)
	base := gitTestOutput(t, root, "rev-parse", "HEAD")
	baseFile, found, err := (Repo{Root: root}).ReadFileAt(ctx, base, "app.txt")
	if err != nil || !found || string(baseFile) != "base\n" {
		t.Fatalf("base file = %q, %v, %v", baseFile, found, err)
	}
	if _, found, err := (Repo{Root: root}).ReadFileAt(ctx, base, "missing.txt"); err != nil || found {
		t.Fatalf("missing file = found %v, error %v", found, err)
	}
	if _, _, err := (Repo{Root: root}).ReadFileAt(ctx, base, "../outside"); err == nil {
		t.Fatal("expected parent traversal path to fail")
	}
	gitTest(t, root, "switch", "-c", "feature")
	writeGitFile(t, filepath.Join(root, "app.txt"), "base\nfeature\n")
	gitTest(t, root, "add", "app.txt")
	gitTest(t, root, "commit", "-m", "feat(app): add feature")
	head := gitTestOutput(t, root, "rev-parse", "HEAD")

	repo, err := Discover(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := repo.ResolveTarget(ctx, TargetOptions{Base: "main", RequireClean: true})
	if err != nil {
		t.Fatal(err)
	}
	if branch.Mode != "branch" || branch.BaseSHA != base || branch.HeadSHA != head || !branch.Finalizable {
		t.Fatalf("unexpected branch target: %#v", branch)
	}
	valid, err := repo.VerifyTarget(ctx, branch)
	if err != nil || !valid {
		t.Fatalf("verify target = %v, %v", valid, err)
	}
	changedPaths, err := repo.ChangedPaths(ctx, branch)
	if err != nil || len(changedPaths) != 1 || changedPaths[0] != "app.txt" {
		t.Fatalf("changed paths = %v, %v", changedPaths, err)
	}

	commit, err := repo.ResolveTarget(ctx, TargetOptions{Commit: "HEAD", RequireClean: true})
	if err != nil || commit.Mode != "commit" || commit.DiffHash != branch.DiffHash {
		t.Fatalf("unexpected commit target: %#v, %v", commit, err)
	}
	rangeTarget, err := repo.ResolveTarget(ctx, TargetOptions{Range: "main..HEAD", RequireClean: true})
	if err != nil || rangeTarget.Mode != "range" || rangeTarget.DiffHash != branch.DiffHash {
		t.Fatalf("unexpected range target: %#v, %v", rangeTarget, err)
	}

	gitTest(t, root, "switch", "main")
	workspace, err := repo.PrepareWorkspace(ctx, commit)
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Root == repo.Root {
		t.Fatal("arbitrary commit should use a temporary worktree")
	}
	workspaceHead := gitTestOutput(t, workspace.Root, "rev-parse", "HEAD")
	if workspaceHead != head {
		t.Fatalf("workspace HEAD = %s, want %s", workspaceHead, head)
	}
	workspaceRoot := workspace.Root
	if err := workspace.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspaceRoot); !os.IsNotExist(err) {
		t.Fatalf("temporary workspace was not removed: %v", err)
	}

	writeGitFile(t, filepath.Join(root, "untracked.txt"), "one\n")
	if _, err := repo.ResolveTarget(ctx, TargetOptions{Base: "main", RequireClean: true}); err == nil {
		t.Fatal("expected clean-tree enforcement")
	}
	worktree, err := repo.ResolveTarget(ctx, TargetOptions{Uncommitted: true})
	if err != nil {
		t.Fatal(err)
	}
	if worktree.Finalizable || worktree.Mode != "uncommitted" {
		t.Fatalf("unexpected worktree target: %#v", worktree)
	}
	changedPaths, err = repo.ChangedPaths(ctx, worktree)
	if err != nil || len(changedPaths) != 1 || changedPaths[0] != "untracked.txt" {
		t.Fatalf("uncommitted changed paths = %v, %v", changedPaths, err)
	}
	diff, err := repo.ReviewDiff(ctx, worktree)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff) == 0 {
		t.Fatal("working-tree review diff is empty")
	}
	writeGitFile(t, filepath.Join(root, "untracked.txt"), "two\n")
	valid, err = repo.VerifyTarget(ctx, worktree)
	if err != nil || valid {
		t.Fatalf("changed worktree should invalidate fingerprint: %v, %v", valid, err)
	}
}

func newGitRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gitTest(t, root, "init", "-b", "main")
	gitTest(t, root, "config", "user.name", "CORA Test")
	gitTest(t, root, "config", "user.email", "cora@example.invalid")
	writeGitFile(t, filepath.Join(root, "app.txt"), "base\n")
	gitTest(t, root, "add", "app.txt")
	gitTest(t, root, "commit", "-m", "chore: initialize test repository")
	return root
}

func gitTest(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func gitTestOutput(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	if len(output) > 0 && output[len(output)-1] == '\n' {
		output = output[:len(output)-1]
	}
	return string(output)
}

func writeGitFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
