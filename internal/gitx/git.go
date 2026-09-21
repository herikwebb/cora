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
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/herikwebb/cora/internal/model"
	processx "github.com/herikwebb/cora/internal/process"
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
	linked     bool
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
	if workingTreeTarget(target) {
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

// PrepareDisposableWorkspace creates an independent local clone with no
// remotes. Reviewers and checks can modify files or Git state there without
// mutating the user's checkout or the source repository's shared Git data.
func (r Repo) PrepareDisposableWorkspace(ctx context.Context, target model.Target) (Workspace, error) {
	var patch []byte
	var err error
	if workingTreeTarget(target) {
		patch, err = r.ReviewDiff(ctx, target)
		if err != nil {
			return Workspace{}, err
		}
	}
	return r.PrepareDisposableWorkspaceSnapshot(ctx, target, patch)
}

// PrepareDisposableWorkspaceSnapshot creates a disposable workspace from the
// supplied immutable patch. Callers that already captured and fingerprinted a
// working-tree review must use this method so concurrent source-tree edits
// cannot make different reviewers inspect different content.
func (r Repo) PrepareDisposableWorkspaceSnapshot(ctx context.Context, target model.Target, patch []byte) (Workspace, error) {
	headSHA := target.HeadSHA
	if workingTreeTarget(target) {
		headSHA = target.BaseSHA
	}
	workspace, err := r.createDisposableClone(ctx, headSHA)
	if err != nil {
		return Workspace{}, err
	}
	if !workingTreeTarget(target) {
		return workspace, nil
	}
	err = gitInput(ctx, workspace.Root, patch, "apply", "--binary", "--whitespace=nowarn", "-")
	if err != nil {
		_ = workspace.Close(context.Background())
		return Workspace{}, fmt.Errorf("materialize working-tree snapshot: %w", err)
	}
	return workspace, nil
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
	return Workspace{Root: worktree, repository: r, parent: parent, temporary: true, linked: true}, nil
}

func (r Repo) createDisposableClone(ctx context.Context, headSHA string) (Workspace, error) {
	parent, err := os.MkdirTemp("", "cora-disposable-")
	if err != nil {
		return Workspace{}, fmt.Errorf("create disposable workspace directory: %w", err)
	}
	workspace := Workspace{Root: filepath.Join(parent, "workspace"), repository: r, parent: parent, temporary: true}
	cleanup := func(cause error) (Workspace, error) {
		_ = os.RemoveAll(parent)
		return Workspace{}, cause
	}
	if _, err := gitBytes(ctx, parent, "clone", "--quiet", "--no-checkout", "--no-hardlinks", "--", r.Root, workspace.Root); err != nil {
		return cleanup(fmt.Errorf("clone disposable workspace: %w", err))
	}
	if _, err := gitBytes(ctx, workspace.Root, "remote", "remove", "origin"); err != nil {
		return cleanup(fmt.Errorf("remove disposable workspace remote: %w", err))
	}
	if _, err := gitBytes(ctx, workspace.Root, "checkout", "--quiet", "--detach", "--force", headSHA); err != nil {
		return cleanup(fmt.Errorf("checkout disposable workspace: %w", err))
	}
	return workspace, nil
}

func (w *Workspace) Close(ctx context.Context) error {
	if w == nil || !w.temporary {
		return nil
	}
	directoryName := "workspace"
	if w.linked {
		directoryName = "worktree"
	}
	if w.parent == "" || w.Root != filepath.Join(w.parent, directoryName) {
		return errors.New("refusing to remove an invalid temporary review workspace")
	}
	// Cleanup must survive cancellation of the operation that created or used
	// the workspace. Bound the detached cleanup so it cannot hang shutdown.
	if ctx == nil {
		ctx = context.Background()
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	var removeErr error
	if w.linked {
		_, removeErr = gitBytes(cleanupCtx, w.repository.Root, "worktree", "remove", "--force", w.Root)
	}
	filesystemErr := processx.RemoveAllWritable(w.parent)
	if removeErr != nil {
		return fmt.Errorf("remove temporary Git worktree: %w", removeErr)
	}
	if filesystemErr != nil {
		return fmt.Errorf("remove temporary review directory: %w", filesystemErr)
	}
	w.temporary = false
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

// ResolveAutoFixTarget fingerprints the complete current working tree against
// the original review base. Unlike the general --uncommitted mode, this exact
// snapshot can be approved and subsequently verified by the auto-fix loop.
func (r Repo) ResolveAutoFixTarget(ctx context.Context, baseRef, baseSHA, expectedHeadSHA string) (model.Target, error) {
	head, err := r.ResolveRevision(ctx, "HEAD")
	if err != nil {
		return model.Target{}, err
	}
	if head != expectedHeadSHA {
		return model.Target{}, errors.New("auto-fix agent changed HEAD or Git branch state")
	}
	patch, err := r.workingTreeDiff(ctx, baseSHA)
	if err != nil {
		return model.Target{}, err
	}
	if len(patch) == 0 {
		return model.Target{}, errors.New("auto-fix working tree has no changes relative to the original base")
	}
	sum := sha256.Sum256(patch)
	return model.Target{
		Mode: "working-tree", BaseRef: baseRef, HeadRef: "working-tree",
		BaseSHA: baseSHA, HeadSHA: head, DiffHash: hex.EncodeToString(sum[:]),
		Dirty: true, Finalizable: true,
	}, nil
}

func (r Repo) CurrentBranch(ctx context.Context) (string, error) {
	branch, err := gitOutput(ctx, r.Root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || branch == "" {
		return "", errors.New("auto-fix requires a checked-out feature branch")
	}
	return branch, nil
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
	// Status is observational here. Disable Git's optional locks so it cannot
	// refresh and rewrite the index while resolving a review or read-only plan.
	status, err := gitOutput(ctx, r.Root, "--no-optional-locks", "status", "--porcelain=v1", "--untracked-files=normal")
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
	if target.Mode == "working-tree" {
		current, err := r.ResolveAutoFixTarget(ctx, target.BaseRef, target.BaseSHA, target.HeadSHA)
		return err == nil && current.DiffHash == target.DiffHash, err
	}
	current, _, err := r.diffHash(ctx, target.BaseSHA, target.HeadSHA)
	return current == target.DiffHash, err
}

// ReviewDiff returns the exact patch supplied to reviewers and stored in the
// audit record. Untracked working-tree files are represented as new files.
func (r Repo) ReviewDiff(ctx context.Context, target model.Target) ([]byte, error) {
	if !workingTreeTarget(target) {
		diff, err := gitBytes(ctx, r.Root, "--no-optional-locks", "diff", "--binary", "--no-ext-diff", "--no-textconv", target.BaseSHA, target.HeadSHA)
		if err != nil {
			return nil, fmt.Errorf("render review diff: %w", err)
		}
		return diff, nil
	}

	return r.workingTreeDiff(ctx, target.BaseSHA)
}

func (r Repo) workingTreeDiff(ctx context.Context, baseSHA string) (diff []byte, returnErr error) {
	environment, cleanup, err := r.readOnlyIndexEnvironment(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, cleanup()) }()
	diff, err = gitBytesEnv(ctx, r.Root, environment, "diff", "--binary", "--no-ext-diff", "--no-textconv", baseSHA)
	if err != nil {
		return nil, fmt.Errorf("render working tree diff: %w", err)
	}
	untrackedRaw, err := gitBytesEnv(ctx, r.Root, environment, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, fmt.Errorf("list untracked files: %w", err)
	}
	untracked := splitNUL(untrackedRaw)
	sort.Strings(untracked)
	for _, name := range untracked {
		fileDiff, stderr, commandResult := gitCapture(ctx, r.Root, nil, "diff", "--no-index", "--binary", "--no-ext-diff", "--no-textconv", "--src-prefix=a/", "--dst-prefix=b/", "--", "/dev/null", name)
		if commandResult.Err != nil && commandResult.ExitCode != 1 {
			return nil, fmt.Errorf("render untracked file %s: %s", name, firstGitError(string(stderr), commandResult.Err))
		}
		diff = append(diff, fileDiff...)
	}
	return diff, nil
}

// ChangedPaths returns the repository-relative paths represented by a target.
func (r Repo) ChangedPaths(ctx context.Context, target model.Target) (paths []string, returnErr error) {
	var raw []byte
	var err error
	var environment []string
	if workingTreeTarget(target) {
		var cleanup func() error
		var environmentErr error
		environment, cleanup, environmentErr = r.readOnlyIndexEnvironment(ctx)
		if environmentErr != nil {
			return nil, environmentErr
		}
		defer func() { returnErr = errors.Join(returnErr, cleanup()) }()
		raw, err = gitBytesEnv(ctx, r.Root, environment, "diff", "--name-status", "-z", "--find-renames", "--no-ext-diff", target.BaseSHA)
	} else {
		raw, err = gitBytes(ctx, r.Root, "--no-optional-locks", "diff", "--name-status", "-z", "--find-renames", "--no-ext-diff", target.BaseSHA, target.HeadSHA)
	}
	if err != nil {
		return nil, fmt.Errorf("list changed paths: %w", err)
	}
	paths, err = parseNameStatusPaths(raw)
	if err != nil {
		return nil, fmt.Errorf("parse changed paths: %w", err)
	}
	if workingTreeTarget(target) {
		untrackedRaw, untrackedErr := gitBytesEnv(ctx, r.Root, environment, "ls-files", "--others", "--exclude-standard", "-z")
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

func parseNameStatusPaths(raw []byte) ([]string, error) {
	fields := splitNUL(raw)
	paths := make([]string, 0, len(fields)/2)
	for index := 0; index < len(fields); {
		status := fields[index]
		index++
		pathCount := 1
		if status[0] == 'R' || status[0] == 'C' {
			pathCount = 2
		}
		if len(fields)-index < pathCount {
			return nil, fmt.Errorf("Git name-status entry %q is missing %d path(s)", status, pathCount-(len(fields)-index))
		}
		paths = append(paths, fields[index:index+pathCount]...)
		index += pathCount
	}
	return paths, nil
}

func workingTreeTarget(target model.Target) bool {
	return target.Mode == "uncommitted" || target.Mode == "working-tree"
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
	_, _, probe := gitCapture(ctx, r.Root, nil, "cat-file", "-e", object)
	if probe.Err != nil {
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
	diff, err := gitBytes(ctx, r.Root, "--no-optional-locks", "diff", "--binary", "--no-ext-diff", "--no-textconv", base, head)
	if err != nil {
		return "", false, fmt.Errorf("calculate diff: %w", err)
	}
	sum := sha256.Sum256(diff)
	return hex.EncodeToString(sum[:]), len(diff) == 0, nil
}

func (r Repo) worktreeHash(ctx context.Context) (hash string, returnErr error) {
	environment, cleanup, err := r.readOnlyIndexEnvironment(ctx)
	if err != nil {
		return "", err
	}
	defer func() { returnErr = errors.Join(returnErr, cleanup()) }()
	tracked, err := gitBytesEnv(ctx, r.Root, environment, "diff", "--binary", "--no-ext-diff", "--no-textconv", "HEAD")
	if err != nil {
		return "", fmt.Errorf("calculate working tree diff: %w", err)
	}
	status, err := gitBytesEnv(ctx, r.Root, environment, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return "", fmt.Errorf("calculate working tree status: %w", err)
	}
	untrackedRaw, err := gitBytesEnv(ctx, r.Root, environment, "ls-files", "--others", "--exclude-standard", "-z")
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

// readOnlyIndexEnvironment gives observational working-tree commands a private
// copy of the repository index. Git may refresh stat information in the index
// even for commands such as diff, so GIT_OPTIONAL_LOCKS=0 alone is not enough
// to keep a plan operation repository-read-only.
func (r Repo) readOnlyIndexEnvironment(ctx context.Context) ([]string, func() error, error) {
	indexName, err := gitOutput(ctx, r.Root, "--no-optional-locks", "rev-parse", "--git-path", "index")
	if err != nil {
		return nil, nil, fmt.Errorf("resolve Git index: %w", err)
	}
	if !filepath.IsAbs(indexName) {
		indexName = filepath.Join(r.Root, indexName)
	}
	indexName = filepath.Clean(indexName)
	indexContents, err := os.ReadFile(indexName)
	if err != nil {
		return nil, nil, fmt.Errorf("read Git index: %w", err)
	}

	temporaryDirectory, err := os.MkdirTemp("", "cora-read-index-")
	if err != nil {
		return nil, nil, fmt.Errorf("create temporary Git index directory: %w", err)
	}
	cleanup := func() error {
		if cleanupErr := processx.RemoveAllWritable(temporaryDirectory); cleanupErr != nil {
			return fmt.Errorf("remove temporary Git index directory: %w", cleanupErr)
		}
		return nil
	}
	fail := func(cause error) ([]string, func() error, error) {
		return nil, nil, errors.Join(cause, cleanup())
	}

	temporaryIndex := filepath.Join(temporaryDirectory, "index")
	if err := os.WriteFile(temporaryIndex, indexContents, 0o600); err != nil {
		return fail(fmt.Errorf("copy Git index: %w", err))
	}
	// A split index references sharedindex.* alongside the primary index. Copy
	// those immutable companions as well so repositories using split-index mode
	// retain the same semantics under GIT_INDEX_FILE.
	entries, err := os.ReadDir(filepath.Dir(indexName))
	if err != nil {
		return fail(fmt.Errorf("inspect Git index directory: %w", err))
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "sharedindex.") {
			continue
		}
		contents, readErr := os.ReadFile(filepath.Join(filepath.Dir(indexName), entry.Name()))
		if readErr != nil {
			return fail(fmt.Errorf("read shared Git index %s: %w", entry.Name(), readErr))
		}
		if writeErr := os.WriteFile(filepath.Join(temporaryDirectory, entry.Name()), contents, 0o600); writeErr != nil {
			return fail(fmt.Errorf("copy shared Git index %s: %w", entry.Name(), writeErr))
		}
	}

	environment := make([]string, 0, len(os.Environ())+2)
	for _, value := range os.Environ() {
		name, _, found := strings.Cut(value, "=")
		if found && (strings.EqualFold(name, "GIT_INDEX_FILE") || strings.EqualFold(name, "GIT_OPTIONAL_LOCKS")) {
			continue
		}
		environment = append(environment, value)
	}
	environment = append(environment, "GIT_INDEX_FILE="+temporaryIndex, "GIT_OPTIONAL_LOCKS=0")
	return environment, cleanup, nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	output, err := gitBytes(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func gitBytes(ctx context.Context, dir string, args ...string) ([]byte, error) {
	output, stderr, result := gitCapture(ctx, dir, nil, args...)
	if result.Err != nil {
		return nil, errors.New(firstGitError(string(stderr), result.Err))
	}
	return output, nil
}

func gitBytesEnv(ctx context.Context, dir string, environment []string, args ...string) ([]byte, error) {
	output, stderr, result := gitCaptureEnv(ctx, dir, environment, nil, args...)
	if result.Err != nil {
		return nil, errors.New(firstGitError(string(stderr), result.Err))
	}
	return output, nil
}

func gitInput(ctx context.Context, dir string, input []byte, args ...string) error {
	_, stderr, result := gitCapture(ctx, dir, input, args...)
	if result.Err != nil {
		return errors.New(firstGitError(string(stderr), result.Err))
	}
	return nil
}

func gitCapture(ctx context.Context, dir string, input []byte, args ...string) ([]byte, []byte, processx.Result) {
	return gitCaptureEnv(ctx, dir, nil, input, args...)
}

func gitCaptureEnv(ctx context.Context, dir string, environment []string, input []byte, args ...string) ([]byte, []byte, processx.Result) {
	commandArgs := append([]string{"-C", dir}, args...)
	return processx.CaptureInput(ctx, "git", dir, environment, input, commandArgs...)
}

func firstGitError(stderr string, err error) string {
	if message := strings.TrimSpace(stderr); message != "" {
		return message
	}
	return err.Error()
}
