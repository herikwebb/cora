package gitx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/herikwebb/cora/internal/model"
)

type Repo struct {
	Root      string
	CommonDir string
}

type Workspace struct {
	Root       string
	repository Repo
	parent     string
	temporary  bool
}

type TargetOptions struct {
	Base         string
	Commit       string
	Range        string
	Uncommitted  bool
	Parent       int
	RequireClean bool
}

func Discover(ctx context.Context, start string) (Repo, error) {
	if start == "" {
		start = "."
	}
	abs, err := filepath.Abs(start)
	if err != nil {
		return Repo{}, fmt.Errorf("resolve working directory: %w", err)
	}
	root, err := gitOutput(ctx, abs, "rev-parse", "--show-toplevel")
	if err != nil {
		return Repo{}, errors.New("CORA must run inside a Git repository")
	}
	common, err := gitOutput(ctx, root, "rev-parse", "--git-common-dir")
	if err != nil {
		return Repo{}, fmt.Errorf("resolve Git common directory: %w", err)
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(root, common)
	}
	common, err = filepath.Abs(common)
	if err != nil {
		return Repo{}, fmt.Errorf("resolve Git common directory: %w", err)
	}
	return Repo{Root: filepath.Clean(root), CommonDir: filepath.Clean(common)}, nil
}

func (r Repo) ResolveTarget(ctx context.Context, opts TargetOptions) (model.Target, error) {
	selected := 0
	if opts.Commit != "" {
		selected++
	}
	if opts.Range != "" {
		selected++
	}
	if opts.Uncommitted {
		selected++
	}
	if selected > 1 {
		return model.Target{}, errors.New("--commit, --range, and --uncommitted are mutually exclusive")
	}

	dirty, err := r.IsDirty(ctx)
	if err != nil {
		return model.Target{}, err
	}
	if opts.RequireClean && dirty && !opts.Uncommitted {
		return model.Target{}, errors.New("working tree is not clean; commit or stash changes, or use --uncommitted")
	}

	switch {
	case opts.Uncommitted:
		return r.resolveUncommitted(ctx, dirty)
	case opts.Commit != "":
		return r.resolveCommit(ctx, opts.Commit, opts.Parent, dirty)
	case opts.Range != "":
		return r.resolveRange(ctx, opts.Range, dirty)
	default:
		return r.resolveBranch(ctx, opts.Base, dirty)
	}
}

// PrepareWorkspace returns a checkout whose HEAD and files match the target.
// It reuses the caller's clean checkout when possible and creates a detached
// temporary Git worktree for an arbitrary commit or dirty caller checkout.
func (r Repo) PrepareWorkspace(ctx context.Context, target model.Target) (Workspace, error) {
	if target.Mode == "uncommitted" {
		return Workspace{Root: r.Root, repository: r}, nil
	}
	currentHead, err := r.ResolveRevision(ctx, "HEAD")
	if err != nil {
		return Workspace{}, err
	}
	if !target.Dirty && currentHead == target.HeadSHA {
		return Workspace{Root: r.Root, repository: r}, nil
	}
	return r.createTemporaryWorkspace(ctx, target.HeadSHA)
}

// PrepareDisposableWorkspace always creates a detached worktree. Checks can
// modify or delete files there without mutating the user's checkout.
func (r Repo) PrepareDisposableWorkspace(ctx context.Context, target model.Target) (Workspace, error) {
	if target.Mode == "uncommitted" {
		return Workspace{}, errors.New("disposable checks require a committed target")
	}
	return r.createTemporaryWorkspace(ctx, target.HeadSHA)
}

func (r Repo) createTemporaryWorkspace(ctx context.Context, headSHA string) (Workspace, error) {
	parent, err := os.MkdirTemp("", "cora-review-")
	if err != nil {
		return Workspace{}, fmt.Errorf("create temporary review directory: %w", err)
	}
	worktree := filepath.Join(parent, "worktree")
	if _, err := gitBytes(ctx, r.Root, "worktree", "add", "--detach", worktree, headSHA); err != nil {
		_ = os.RemoveAll(parent)
		return Workspace{}, fmt.Errorf("create temporary review worktree: %w", err)
	}
	return Workspace{Root: worktree, repository: r, parent: parent, temporary: true}, nil
}

func (w *Workspace) Close(ctx context.Context) error {
	if w == nil || !w.temporary {
		return nil
	}
	if w.parent == "" || w.Root != filepath.Join(w.parent, "worktree") {
		return errors.New("refusing to remove an invalid temporary review workspace")
	}
	_, removeErr := gitBytes(ctx, w.repository.Root, "worktree", "remove", "--force", w.Root)
	filesystemErr := os.RemoveAll(w.parent)
	w.temporary = false
	if removeErr != nil {
		return fmt.Errorf("remove temporary Git worktree: %w", removeErr)
	}
	if filesystemErr != nil {
		return fmt.Errorf("remove temporary review directory: %w", filesystemErr)
	}
	return nil
}

func (r Repo) resolveBranch(ctx context.Context, base string, dirty bool) (model.Target, error) {
	head, err := r.ResolveRevision(ctx, "HEAD")
	if err != nil {
		return model.Target{}, err
	}
	if strings.TrimSpace(base) == "" {
		base, err = r.DetectBase(ctx)
		if err != nil {
			return model.Target{}, err
		}
	}
	baseTip, err := r.ResolveRevision(ctx, base)
	if err != nil {
		return model.Target{}, fmt.Errorf("resolve base %q: %w", base, err)
	}
	mergeBase, err := gitOutput(ctx, r.Root, "merge-base", baseTip, head)
	if err != nil {
		return model.Target{}, fmt.Errorf("find merge base for %s and HEAD: %w", base, err)
	}
	diffHash, empty, err := r.diffHash(ctx, mergeBase, head)
	if err != nil {
		return model.Target{}, err
	}
	if empty {
		return model.Target{}, fmt.Errorf("no changes between %s and HEAD", base)
	}
	return model.Target{
		Mode:        "branch",
		BaseRef:     base,
		HeadRef:     "HEAD",
		BaseSHA:     mergeBase,
		HeadSHA:     head,
		DiffHash:    diffHash,
		Dirty:       dirty,
		Finalizable: !dirty,
	}, nil
}

func (r Repo) resolveCommit(ctx context.Context, revision string, parent int, dirty bool) (model.Target, error) {
	head, err := r.ResolveRevision(ctx, revision)
	if err != nil {
		return model.Target{}, fmt.Errorf("resolve commit %q: %w", revision, err)
	}
	parentsText, err := gitOutput(ctx, r.Root, "rev-list", "--parents", "-n", "1", head)
	if err != nil {
		return model.Target{}, fmt.Errorf("resolve parents for %s: %w", revision, err)
	}
	parts := strings.Fields(parentsText)
	parents := parts[1:]
	if len(parents) == 0 {
		return model.Target{}, errors.New("root commits are not supported by --commit yet; use --range")
	}
	if len(parents) > 1 && parent == 0 {
		return model.Target{}, errors.New("merge commit requires --parent")
	}
	if parent == 0 {
		parent = 1
	}
	if parent < 1 || parent > len(parents) {
		return model.Target{}, fmt.Errorf("parent must be between 1 and %d", len(parents))
	}
	base := parents[parent-1]
	diffHash, empty, err := r.diffHash(ctx, base, head)
	if err != nil {
		return model.Target{}, err
	}
	if empty {
		return model.Target{}, fmt.Errorf("commit %s has no changes relative to parent %d", revision, parent)
	}
	return model.Target{
		Mode:        "commit",
		BaseRef:     fmt.Sprintf("%s^%d", revision, parent),
		HeadRef:     revision,
		BaseSHA:     base,
		HeadSHA:     head,
		DiffHash:    diffHash,
		Dirty:       dirty,
		Finalizable: !dirty,
	}, nil
}

func (r Repo) resolveRange(ctx context.Context, revisionRange string, dirty bool) (model.Target, error) {
	if strings.Contains(revisionRange, "...") || strings.Count(revisionRange, "..") != 1 {
		return model.Target{}, errors.New("--range must use the form BASE..HEAD")
	}
	baseRef, headRef, _ := strings.Cut(revisionRange, "..")
	if strings.TrimSpace(baseRef) == "" || strings.TrimSpace(headRef) == "" {
		return model.Target{}, errors.New("--range must use the form BASE..HEAD")
	}
	base, err := r.ResolveRevision(ctx, baseRef)
	if err != nil {
		return model.Target{}, fmt.Errorf("resolve range base %q: %w", baseRef, err)
	}
	head, err := r.ResolveRevision(ctx, headRef)
	if err != nil {
		return model.Target{}, fmt.Errorf("resolve range head %q: %w", headRef, err)
	}
	diffHash, empty, err := r.diffHash(ctx, base, head)
	if err != nil {
		return model.Target{}, err
	}
	if empty {
		return model.Target{}, fmt.Errorf("range %s has no changes", revisionRange)
	}
	return model.Target{
		Mode:        "range",
		BaseRef:     baseRef,
		HeadRef:     headRef,
		BaseSHA:     base,
		HeadSHA:     head,
		DiffHash:    diffHash,
		Dirty:       dirty,
		Finalizable: !dirty,
	}, nil
}

func (r Repo) resolveUncommitted(ctx context.Context, dirty bool) (model.Target, error) {
	if !dirty {
		return model.Target{}, errors.New("working tree has no uncommitted changes")
	}
	head, err := r.ResolveRevision(ctx, "HEAD")
	if err != nil {
		return model.Target{}, err
	}
	hash, err := r.worktreeHash(ctx)
	if err != nil {
		return model.Target{}, err
	}
	return model.Target{
		Mode:        "uncommitted",
		BaseRef:     "HEAD",
		HeadRef:     "working-tree",
		BaseSHA:     head,
		HeadSHA:     head,
		DiffHash:    hash,
		Dirty:       true,
		Finalizable: false,
	}, nil
}

func (r Repo) DetectBase(ctx context.Context) (string, error) {
	for _, remote := range []string{"upstream", "origin"} {
		ref, err := gitOutput(ctx, r.Root, "symbolic-ref", "--quiet", "--short", "refs/remotes/"+remote+"/HEAD")
		if err == nil && ref != "" {
			return ref, nil
		}
	}
	for _, candidate := range []string{"upstream/main", "origin/main", "main", "master"} {
		if _, err := r.ResolveRevision(ctx, candidate); err == nil {
			return candidate, nil
		}
	}
	return "", errors.New("could not detect a base branch; pass --base or configure base")
}

// StableIdentity returns a credential-free remote identity such as
// github.com/owner/repository. Repositories without a remote fall back to a
// root-commit identity that remains stable across clones of the same history.
func (r Repo) StableIdentity(ctx context.Context) (string, error) {
	for _, remote := range []string{"origin", "upstream"} {
		value, err := gitOutput(ctx, r.Root, "config", "--get", "remote."+remote+".url")
		if err == nil && strings.TrimSpace(value) != "" {
			if identity := normalizeRemoteIdentity(value); identity != "" {
				return identity, nil
			}
		}
	}
	roots, err := gitOutput(ctx, r.Root, "rev-list", "--max-parents=0", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve repository identity: %w", err)
	}
	root := strings.Fields(roots)
	if len(root) == 0 {
		return "", errors.New("resolve repository identity: repository has no root commit")
	}
	sort.Strings(root)
	return "git:" + root[0], nil
}

func normalizeRemoteIdentity(remote string) string {
	remote = strings.TrimSpace(remote)
	if at := strings.LastIndex(remote, "@"); at >= 0 && !strings.Contains(remote[:at], "://") {
		if colon := strings.Index(remote[at+1:], ":"); colon >= 0 {
			host := remote[at+1 : at+1+colon]
			path := remote[at+1+colon+1:]
			return cleanRemoteIdentity(host, path)
		}
	}
	parsed, err := url.Parse(remote)
	if err == nil && parsed.Hostname() != "" {
		return cleanRemoteIdentity(parsed.Hostname(), parsed.Path)
	}
	return ""
}

func cleanRemoteIdentity(host, repositoryPath string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	repositoryPath = strings.Trim(strings.TrimSpace(repositoryPath), "/")
	repositoryPath = strings.TrimSuffix(repositoryPath, ".git")
	if host == "" || repositoryPath == "" || strings.Contains(repositoryPath, "..") {
		return ""
	}
	return host + "/" + repositoryPath
}

func (r Repo) ResolveRevision(ctx context.Context, revision string) (string, error) {
	if strings.HasPrefix(revision, "-") {
		return "", errors.New("revision cannot begin with '-'")
	}
	output, err := gitOutput(ctx, r.Root, "rev-parse", "--verify", revision+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("unknown revision %q", revision)
	}
	return output, nil
}

func (r Repo) IsDirty(ctx context.Context) (bool, error) {
	status, err := gitOutput(ctx, r.Root, "status", "--porcelain=v1", "--untracked-files=normal")
	if err != nil {
		return false, fmt.Errorf("read Git status: %w", err)
	}
	return status != "", nil
}

func (r Repo) VerifyTarget(ctx context.Context, target model.Target) (bool, error) {
	if target.Mode == "uncommitted" {
		current, err := r.worktreeHash(ctx)
		return current == target.DiffHash, err
	}
	current, _, err := r.diffHash(ctx, target.BaseSHA, target.HeadSHA)
	return current == target.DiffHash, err
}

// ReviewDiff returns the exact patch supplied to reviewers and stored in the
// audit record. Untracked working-tree files are represented as new files.
func (r Repo) ReviewDiff(ctx context.Context, target model.Target) ([]byte, error) {
	if target.Mode != "uncommitted" {
		diff, err := gitBytes(ctx, r.Root, "diff", "--binary", "--no-ext-diff", "--no-textconv", target.BaseSHA, target.HeadSHA)
		if err != nil {
			return nil, fmt.Errorf("render review diff: %w", err)
		}
		return diff, nil
	}

	diff, err := gitBytes(ctx, r.Root, "diff", "--binary", "--no-ext-diff", "--no-textconv", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("render working tree diff: %w", err)
	}
	untrackedRaw, err := gitBytes(ctx, r.Root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, fmt.Errorf("list untracked files: %w", err)
	}
	untracked := splitNUL(untrackedRaw)
	sort.Strings(untracked)
	for _, name := range untracked {
		command := exec.CommandContext(ctx, "git", "-C", r.Root, "diff", "--no-index", "--binary", "--no-ext-diff", "--no-textconv", "--src-prefix=a/", "--dst-prefix=b/", "--", "/dev/null", name)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		fileDiff, commandErr := command.Output()
		var exitErr *exec.ExitError
		if commandErr != nil && (!errors.As(commandErr, &exitErr) || exitErr.ExitCode() != 1) {
			return nil, fmt.Errorf("render untracked file %s: %s", name, firstGitError(stderr.String(), commandErr))
		}
		diff = append(diff, fileDiff...)
	}
	return diff, nil
}

// ChangedPaths returns the repository-relative paths represented by a target.
func (r Repo) ChangedPaths(ctx context.Context, target model.Target) ([]string, error) {
	var raw []byte
	var err error
	if target.Mode == "uncommitted" {
		raw, err = gitBytes(ctx, r.Root, "diff", "--name-only", "-z", "--no-ext-diff", "HEAD")
	} else {
		raw, err = gitBytes(ctx, r.Root, "diff", "--name-only", "-z", "--no-ext-diff", target.BaseSHA, target.HeadSHA)
	}
	if err != nil {
		return nil, fmt.Errorf("list changed paths: %w", err)
	}
	paths := splitNUL(raw)
	if target.Mode == "uncommitted" {
		untrackedRaw, untrackedErr := gitBytes(ctx, r.Root, "ls-files", "--others", "--exclude-standard", "-z")
		if untrackedErr != nil {
			return nil, fmt.Errorf("list untracked paths: %w", untrackedErr)
		}
		paths = append(paths, splitNUL(untrackedRaw)...)
	}
	unique := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path != "" {
			unique[filepath.ToSlash(path)] = struct{}{}
		}
	}
	paths = paths[:0]
	for path := range unique {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

// ReadFileAt returns a repository file from an exact Git revision. A missing
// file is reported with found=false; the working tree is never consulted.
func (r Repo) ReadFileAt(ctx context.Context, revision, name string) ([]byte, bool, error) {
	clean := pathpkg.Clean(strings.ReplaceAll(name, "\\", "/"))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || strings.ContainsRune(clean, '\x00') {
		return nil, false, fmt.Errorf("invalid repository path %q", name)
	}
	if _, err := r.ResolveRevision(ctx, revision); err != nil {
		return nil, false, err
	}
	object := revision + ":" + clean
	probe := exec.CommandContext(ctx, "git", "-C", r.Root, "cat-file", "-e", object)
	if err := probe.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, false, nil
	}
	contents, err := gitBytes(ctx, r.Root, "cat-file", "blob", object)
	if err != nil {
		return nil, false, fmt.Errorf("read %s at %s: %w", clean, revision, err)
	}
	return contents, true, nil
}

func (r Repo) diffHash(ctx context.Context, base, head string) (string, bool, error) {
	diff, err := gitBytes(ctx, r.Root, "diff", "--binary", "--no-ext-diff", "--no-textconv", base, head)
	if err != nil {
		return "", false, fmt.Errorf("calculate diff: %w", err)
	}
	sum := sha256.Sum256(diff)
	return hex.EncodeToString(sum[:]), len(diff) == 0, nil
}

func (r Repo) worktreeHash(ctx context.Context) (string, error) {
	tracked, err := gitBytes(ctx, r.Root, "diff", "--binary", "--no-ext-diff", "--no-textconv", "HEAD")
	if err != nil {
		return "", fmt.Errorf("calculate working tree diff: %w", err)
	}
	status, err := gitBytes(ctx, r.Root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return "", fmt.Errorf("calculate working tree status: %w", err)
	}
	untrackedRaw, err := gitBytes(ctx, r.Root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", fmt.Errorf("list untracked files: %w", err)
	}
	untracked := splitNUL(untrackedRaw)
	sort.Strings(untracked)

	hasher := sha256.New()
	_, _ = hasher.Write(tracked)
	_, _ = hasher.Write(status)
	for _, name := range untracked {
		if strings.ContainsRune(name, '\x00') {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(r.Root, filepath.FromSlash(name)))
		if err != nil {
			return "", fmt.Errorf("hash untracked file %s: %w", name, err)
		}
		_, _ = hasher.Write([]byte(name))
		_, _ = hasher.Write([]byte{0})
		_, _ = hasher.Write(contents)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func splitNUL(data []byte) []string {
	parts := bytes.Split(data, []byte{0})
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) > 0 {
			result = append(result, string(part))
		}
	}
	return result
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	output, err := gitBytes(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func gitBytes(ctx context.Context, dir string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, errors.New(firstGitError(stderr.String(), err))
	}
	return output, nil
}

func firstGitError(stderr string, err error) string {
	if message := strings.TrimSpace(stderr); message != "" {
		return message
	}
	return err.Error()
}
