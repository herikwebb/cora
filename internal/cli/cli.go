package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/herikwebb/cora/internal/autofix"
	"github.com/herikwebb/cora/internal/config"
	"github.com/herikwebb/cora/internal/gitx"
	"github.com/herikwebb/cora/internal/model"
	"github.com/herikwebb/cora/internal/orchestrator"
	"github.com/herikwebb/cora/internal/provider"
	"github.com/herikwebb/cora/internal/record"
	"github.com/herikwebb/cora/internal/webevidence"
	"github.com/spf13/cobra"
)

var Version = "0.1.0-dev"
var SourceSHA = "unknown"
var BuildTime = "unknown"

const stateDeltaApproved = "delta_approved"

type options struct {
	repo string
	json bool
}

type stateError struct {
	state string
}

func (e stateError) Error() string { return e.state }

func Execute() int {
	signals := append([]os.Signal{os.Interrupt}, terminationSignals()...)
	ctx, stop := interruptContext(context.Background(), signals...)
	defer stop()
	return executeContext(ctx)
}

// interruptContext restores the operating system's default signal behavior
// before it cancels the command context. Cleanup gets one graceful signal; a
// second signal can therefore terminate the process if cleanup itself hangs.
func interruptContext(parent context.Context, signals ...os.Signal) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	notifications := make(chan os.Signal, 1)
	signal.Notify(notifications, signals...)
	done := make(chan struct{})
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			signal.Stop(notifications)
			close(done)
			cancel()
		})
	}
	go func() {
		select {
		case <-notifications:
			// Stop first: once observers see cancellation, the next matching
			// signal must use the platform's default behavior.
			signal.Stop(notifications)
			cancel()
		case <-parent.Done():
			signal.Stop(notifications)
			cancel()
		case <-done:
		}
	}()
	return ctx, stop
}

func executeContext(ctx context.Context) int {
	root := newRootCommand()
	root.SilenceErrors = true
	root.SilenceUsage = true
	if err := root.ExecuteContext(ctx); err != nil {
		return commandErrorExitCode(err, os.Stderr)
	}
	return 0
}

func commandErrorExitCode(err error, stderr io.Writer) int {
	// Caller cancellation takes precedence over a joined state or cleanup
	// error so an interrupted command consistently exits like SIGINT.
	if errors.Is(err, context.Canceled) {
		return 130
	}
	var state stateError
	if errors.As(err, &state) {
		return exitCodeForState(state.state)
	}
	fmt.Fprintln(stderr, "cora:", err)
	return 10
}

func newRootCommand() *cobra.Command {
	opts := &options{}
	root := &cobra.Command{
		Use:           "cora",
		Short:         "Consensus-oriented multi-agent code review",
		Version:       Version,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.SetVersionTemplate("cora version {{.Version}} (source " + SourceSHA + ", built " + BuildTime + ")\n")
	root.PersistentFlags().StringVarP(&opts.repo, "repo", "C", ".", "target repository directory")
	root.PersistentFlags().BoolVar(&opts.json, "json", false, "emit machine-readable JSON")
	root.AddCommand(newReviewCommand(opts))
	root.AddCommand(newPlanCommand(opts))
	root.AddCommand(newRetryCommand(opts))
	root.AddCommand(newStatusCommand(opts))
	root.AddCommand(newListCommand(opts))
	root.AddCommand(newShowCommand(opts))
	root.AddCommand(newVerifyCommand(opts))
	root.AddCommand(newConfigCommand(opts))
	root.AddCommand(newCompletionCommand(root))
	return root
}

func newConfigCommand(opts *options) *cobra.Command {
	command := &cobra.Command{
		Use:   "config",
		Short: "Inspect CORA configuration",
	}
	command.AddCommand(&cobra.Command{
		Use:   "path",
		Short: "Print the personal configuration path",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			path, err := config.UserPath()
			if err != nil {
				return err
			}
			if opts.json {
				return printJSON(map[string]string{"path": path})
			}
			fmt.Fprintln(command.OutOrStdout(), path)
			return nil
		},
	})
	return command
}

func newReviewCommand(opts *options) *cobra.Command {
	var base string
	var commit string
	var revisionRange string
	var uncommitted bool
	var parent int
	var allowAPIBilling bool
	var allowReviewWeb bool
	var allowUnsafeChecks bool
	var securitySensitive bool
	var adjudicate bool
	var strict bool
	var profiles []string
	var validationEvidence []string
	var webEvidence []string
	var autoFix bool
	var resumeAutoFix string
	var until string
	var maxIterations int
	var maxDuration time.Duration
	var maxTurns int
	var maxCostUSD float64
	var agentTimeout time.Duration
	command := &cobra.Command{
		Use:   "review",
		Short: "Review changes independently and evaluate consensus",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			ctx := command.Context()
			autoOnlyFlags := []string{"until", "max-iterations", "max-duration", "max-turns", "max-cost-usd", "agent-timeout"}
			if resumeAutoFix != "" && !autoFix {
				return errors.New("--resume requires --auto-fix")
			}
			if !autoFix {
				for _, name := range autoOnlyFlags {
					if command.Flags().Changed(name) {
						return fmt.Errorf("--%s requires --auto-fix", name)
					}
				}
			}
			if autoFix && (commit != "" || revisionRange != "" || uncommitted || parent != 0) {
				return errors.New("--auto-fix supports only the checked-out branch target; do not combine it with --commit, --range, --uncommitted, or --parent")
			}
			if autoFix && len(validationEvidence) > 0 {
				return errors.New("--validation-evidence is bound to one exact diff and cannot be combined with --auto-fix")
			}
			if autoFix && len(webEvidence) > 0 {
				return errors.New("--web-evidence is frozen for one review and cannot be combined with --auto-fix")
			}
			if autoFix && resumeAutoFix == "" && command.Flags().Changed("allow-review-web") {
				return errors.New("--allow-review-web has no effect in auto-fix mode")
			}
			if resumeAutoFix != "" {
				if base != "" {
					return errors.New("--resume uses the recorded base; do not combine it with --base")
				}
				for _, name := range autoOnlyFlags {
					if command.Flags().Changed(name) {
						return fmt.Errorf("--%s cannot change an existing auto-fix loop during --resume", name)
					}
				}
				for _, name := range []string{"allow-api-billing", "allow-review-web", "allow-unsafe-checks", "security-sensitive", "adjudicate", "strict", "profile", "web-evidence"} {
					if command.Flags().Changed(name) {
						return fmt.Errorf("--%s cannot change the recorded review policy during --resume", name)
					}
				}
			}
			if command.Flags().Changed("max-iterations") && maxIterations < 1 {
				return errors.New("--max-iterations must be positive")
			}
			if command.Flags().Changed("max-duration") && maxDuration <= 0 {
				return errors.New("--max-duration must be positive")
			}
			if command.Flags().Changed("max-turns") && maxTurns < 1 {
				return errors.New("--max-turns must be positive")
			}
			if command.Flags().Changed("max-cost-usd") && maxCostUSD <= 0 {
				return errors.New("--max-cost-usd must be positive")
			}
			if command.Flags().Changed("agent-timeout") && agentTimeout <= 0 {
				return errors.New("--agent-timeout must be positive")
			}
			repo, err := gitx.Discover(ctx, opts.repo)
			if err != nil {
				return err
			}
			if resumeAutoFix != "" {
				plan, err := autofix.PrepareResume(ctx, repo, resumeAutoFix)
				if err != nil {
					return err
				}
				personal, err := config.LoadPersonal()
				if err != nil {
					return err
				}
				trustedTarget := plan.CurrentTarget
				trustedTarget.BaseSHA = plan.Loop.BaseSHA
				cfg, err := loadTrustedConfig(ctx, repo, personal, trustedTarget)
				if err != nil {
					return err
				}
				loop, err := (autofix.Runner{
					Reviewer: runner(command), Progress: command.ErrOrStderr(),
					Version: Version, SourceSHA: SourceSHA, BuildTime: BuildTime,
				}).Resume(ctx, repo, resumeAutoFix, cfg)
				if err != nil {
					return err
				}
				if opts.json {
					if err := printJSON(loop); err != nil {
						return err
					}
				} else {
					printAutoFixLoop(command.OutOrStdout(), loop)
				}
				if loop.State != model.StateApproved {
					return stateError{state: loop.State}
				}
				return nil
			}
			personalConfig, err := config.LoadPersonal()
			if err != nil {
				return err
			}
			candidateBase := personalConfig.Base
			if base != "" {
				candidateBase = base
			}
			resolve := func(baseRef string, requireClean bool) (model.Target, error) {
				return repo.ResolveTarget(ctx, gitx.TargetOptions{
					Base:         baseRef,
					Commit:       commit,
					Range:        revisionRange,
					Uncommitted:  uncommitted,
					Parent:       parent,
					RequireClean: requireClean,
				})
			}
			target, err := resolve(candidateBase, false)
			if err != nil {
				return err
			}
			cfg, err := loadTrustedConfig(ctx, repo, personalConfig, target)
			if err != nil {
				return err
			}
			if target.Mode == "branch" && base == "" && cfg.Base != "" && cfg.Base != target.BaseRef {
				candidateBase = cfg.Base
				target, err = resolve(candidateBase, false)
				if err != nil {
					return err
				}
				cfg, err = loadTrustedConfig(ctx, repo, personalConfig, target)
				if err != nil {
					return err
				}
				if cfg.Base != "" && cfg.Base != candidateBase {
					return fmt.Errorf("trusted repository config changes base from %q to %q; pass --base explicitly", candidateBase, cfg.Base)
				}
			}
			if target.Mode == "branch" {
				candidateBase = target.BaseRef
				if base != "" {
					candidateBase = base
				}
				cfg.Base = candidateBase
			}
			applyReviewPolicyOverrides(&cfg, reviewPolicyOverrides{
				AllowAPIBilling: allowAPIBilling, AllowReviewWeb: allowReviewWeb, AllowUnsafeChecks: allowUnsafeChecks,
				SecuritySensitive: securitySensitive, Adjudicate: adjudicate, Strict: strict,
			})
			if until != "" {
				cfg.AutoFix.Threshold = strings.ToLower(strings.TrimSpace(until))
			}
			if maxIterations > 0 {
				cfg.AutoFix.MaxIterations = maxIterations
			}
			if maxDuration > 0 {
				cfg.AutoFix.MaxDuration.Duration = maxDuration
			}
			if maxTurns > 0 {
				cfg.AutoFix.MaxTurns = maxTurns
			}
			if maxCostUSD > 0 {
				cfg.AutoFix.MaxCostUSD = maxCostUSD
			}
			if agentTimeout > 0 {
				cfg.AutoFix.AgentTimeout.Duration = agentTimeout
			}
			if err := cfg.Validate(); err != nil {
				return err
			}
			webEvidence, err = webevidence.ValidateURLs(webEvidence)
			if err != nil {
				return err
			}
			target, err = resolve(candidateBase, cfg.RequireCleanTree || autoFix)
			if err != nil {
				return err
			}
			if len(profiles) == 0 && len(cfg.Checks) == 0 && cfg.AllowUnsafeChecks {
				profiles = []string{"auto"}
			}
			profiles, err = expandAutoProfiles(ctx, repo, target, profiles)
			if err != nil {
				return err
			}
			cfg, err = config.ApplyProfiles(cfg, profiles)
			if err != nil {
				return err
			}
			if autoFix {
				loop, err := (autofix.Runner{
					Reviewer: runner(command), Progress: command.ErrOrStderr(),
					Version: Version, SourceSHA: SourceSHA, BuildTime: BuildTime,
				}).Run(ctx, repo, target, cfg)
				if err != nil {
					return err
				}
				if opts.json {
					if err := printJSON(loop); err != nil {
						return err
					}
				} else {
					printAutoFixLoop(command.OutOrStdout(), loop)
				}
				if loop.State != model.StateApproved {
					return stateError{state: loop.State}
				}
				return nil
			}
			decision, err := runner(command).RunWithOptions(ctx, repo, target, cfg, orchestrator.RunOptions{
				ValidationEvidencePaths: validationEvidence, WebEvidenceURLs: webEvidence,
			})
			if err != nil {
				return err
			}
			if opts.json {
				if err := printJSON(decision); err != nil {
					return err
				}
			} else {
				printDecision(decision)
			}
			if decision.State != model.StateApproved {
				return stateError{state: decision.State}
			}
			return nil
		},
	}
	command.Flags().StringVar(&base, "base", "", "base branch or commit")
	command.Flags().StringVar(&commit, "commit", "", "review one commit")
	command.Flags().StringVar(&revisionRange, "range", "", "review BASE..HEAD")
	command.Flags().BoolVar(&uncommitted, "uncommitted", false, "review working-tree changes")
	command.Flags().IntVar(&parent, "parent", 0, "parent number for a merge commit")
	command.Flags().BoolVar(&allowAPIBilling, "allow-api-billing", false, "allow API-key or other separately billed authentication")
	command.Flags().BoolVar(&allowReviewWeb, "allow-review-web", false, "allow Cora to capture explicitly selected HTTPS evidence before reviewers start")
	command.Flags().BoolVar(&allowUnsafeChecks, "allow-unsafe-checks", false, "allow configured checks to execute unsandboxed on the host")
	command.Flags().BoolVar(&securitySensitive, "security-sensitive", false, "escalate Claude to the configured security review model and effort")
	command.Flags().BoolVar(&adjudicate, "adjudicate", false, "run a Fable adjudicator when reviewers disagree")
	command.Flags().BoolVar(&strict, "strict", false, "treat minor findings as blocking and require validation checks")
	command.Flags().StringSliceVar(&profiles, "profile", nil, "validation profile to run (repeatable; auto detects a built-in profile)")
	command.Flags().StringArrayVar(&validationEvidence, "validation-evidence", nil, "import passed external validation evidence bound to this exact diff (repeatable)")
	command.Flags().StringArrayVar(&webEvidence, "web-evidence", nil, "capture an HTTPS page as immutable untrusted reviewer evidence (repeatable; requires review-web authorization)")
	command.Flags().BoolVar(&autoFix, "auto-fix", false, "iteratively launch a coding agent and re-review qualifying findings")
	command.Flags().StringVar(&resumeAutoFix, "resume", "", "resume a quota-paused auto-fix loop by ID")
	command.Flags().StringVar(&until, "until", "", "auto-fix severity threshold: blocker, major, or minor")
	command.Flags().IntVar(&maxIterations, "max-iterations", 0, "maximum auto-fix review iterations (defaults to configuration)")
	command.Flags().DurationVar(&maxDuration, "max-duration", 0, "maximum total auto-fix duration (defaults to configuration)")
	command.Flags().IntVar(&maxTurns, "max-turns", 0, "maximum cumulative provider turns for auto-fix")
	command.Flags().Float64Var(&maxCostUSD, "max-cost-usd", 0, "maximum cumulative API-equivalent auto-fix cost")
	command.Flags().DurationVar(&agentTimeout, "agent-timeout", 0, "timeout for each coding-agent attempt")
	return command
}

func printAutoFixLoop(writer io.Writer, loop model.AutoFixLoop) {
	fmt.Fprintf(writer, "%s AUTO-FIX %s\n", strings.ToUpper(loop.State), loop.LoopID)
	fmt.Fprintf(writer, "Reason: %s\n", loop.Reason)
	fmt.Fprintf(writer, "Iterations: %d/%d\n", len(loop.Iterations), loop.MaxIterations)
	fmt.Fprintf(writer, "Threshold: %s\n", loop.Threshold)
	fmt.Fprintf(writer, "Final diff: %s\n", shortSHA(loop.FinalDiffHash))
	fmt.Fprintf(writer, "Elapsed: %s\n", formatMilliseconds(loop.Elapsed.Milliseconds()))
	fmt.Fprintf(writer, "Usage: %s\n", formatUsage(loop.Usage))
	if loop.State == model.StatePaused {
		if loop.RetryAt != nil {
			fmt.Fprintf(writer, "Retry after: %s\n", loop.RetryAt.Local().Format(time.RFC3339))
		}
		fmt.Fprintf(writer, "Resume: cora review --auto-fix --resume %s\n", loop.LoopID)
	}
	for _, iteration := range loop.Iterations {
		blockingFindings, nonBlockingFindings, countsKnown := autoFixIterationFindingCounts(loop, iteration)
		fmt.Fprintf(writer, "  iteration %d: review=%s findings: ", iteration.Number, iteration.ReviewState)
		if countsKnown {
			fmt.Fprintf(writer, "blocking=%d, non-blocking=%d", blockingFindings, nonBlockingFindings)
		} else {
			fmt.Fprint(writer, "unknown")
		}
		fmt.Fprintf(writer, " qualifying=%d run=%s", len(iteration.QualifyingFindingIDs), iteration.ReviewRunID)
		if iteration.Fix != nil {
			fmt.Fprintf(writer, " fix=%s", iteration.Fix.Status)
		}
		fmt.Fprintln(writer)
	}
	for _, iteration := range loop.Iterations {
		if iteration.Fix != nil {
			fmt.Fprintln(writer, "Working tree: modified by the coding agent; inspect and commit manually.")
			break
		}
	}
	fmt.Fprintln(writer, "Record:", loop.RecordPath)
}

func autoFixIterationFindingCounts(loop model.AutoFixLoop, iteration model.AutoFixIteration) (blocking, nonBlocking int, known bool) {
	if iteration.BlockingFindings+iteration.NonBlockingFindings > 0 {
		return iteration.BlockingFindings, iteration.NonBlockingFindings, true
	}
	if loop.FinalDecision != nil && loop.FinalDecision.RunID == iteration.ReviewRunID {
		blocking, nonBlocking = decisionFindingCounts(*loop.FinalDecision)
		return blocking, nonBlocking, true
	}
	if iteration.ReviewRecordPath != "" {
		decision, err := record.LoadDecision(record.Run{ID: iteration.ReviewRunID, Path: iteration.ReviewRecordPath})
		if err == nil {
			blocking, nonBlocking = decisionFindingCounts(decision)
			return blocking, nonBlocking, true
		}
	}
	// Historical loop records predate persisted aggregate counts. Reporting an
	// unavailable count as zero is materially misleading, especially when the
	// same iteration retains qualifying finding IDs.
	return 0, 0, false
}

func newRetryCommand(opts *options) *cobra.Command {
	var reviewers []string
	var noWait bool
	var allowAPIBilling bool
	var adjudicate bool
	var strict bool
	var reviewerTimeout time.Duration
	var overallTimeout time.Duration
	var maxTurns int
	command := &cobra.Command{
		Use:   "retry [run-id]",
		Short: "Retry selected reviewers while reusing completed work",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			ctx := command.Context()
			repo, err := gitx.Discover(ctx, opts.repo)
			if err != nil {
				return err
			}
			runID := "latest"
			if len(args) == 1 {
				runID = args[0]
			}
			store := record.New(repo.CommonDir)
			parentRun, err := store.Resolve(runID)
			if err != nil {
				return err
			}
			parentManifest, err := record.LoadManifest(parentRun)
			if err != nil {
				return err
			}
			parentReviewScope, err := webevidence.EffectiveReviewScope(parentManifest)
			if err != nil {
				return fmt.Errorf("run %s has an invalid review scope: %w", parentRun.ID, err)
			}
			if parentReviewScope == "approved-baseline-delta" {
				return fmt.Errorf("run %s approved only an auto-fix delta; resume parent loop %s to perform the required final full review", parentRun.ID, parentManifest.AutoFixLoopID)
			}
			if parentManifest.FinishedAt.IsZero() {
				return fmt.Errorf("run %s is still active or did not finish", parentRun.ID)
			}
			repositoryIdentity, err := repo.StableIdentity(ctx)
			if err != nil {
				return err
			}
			if parentManifest.RepositoryIdentity == "" || parentManifest.RepositoryIdentity != repositoryIdentity {
				return fmt.Errorf("run %s belongs to a different repository identity", parentRun.ID)
			}
			valid, err := repo.VerifyTarget(ctx, parentManifest.Target)
			if err != nil {
				return err
			}
			if !valid {
				return fmt.Errorf("run %s no longer matches its recorded Git target", parentRun.ID)
			}
			if record.IsWebEvidenceRun(parentRun) || parentManifest.WebEvidence != nil {
				if err := store.ValidateReviewArtifacts(parentRun, parentManifest); err != nil {
					return fmt.Errorf("run %s has invalid immutable review inputs: %w", parentRun.ID, err)
				}
			} else {
				if err := record.ValidateImportedValidationEvidence(parentRun, parentManifest.Target, repositoryIdentity, parentManifest.Checks); err != nil {
					return fmt.Errorf("run %s has invalid imported validation evidence: %w", parentRun.ID, err)
				}
				if err := webevidence.ValidateReviewerBindings(nil, parentManifest.Reviewers, parentManifest.SecurityReviews, parentManifest.CrossExaminations); err != nil {
					return fmt.Errorf("run %s has invalid reviewer bindings: %w", parentRun.ID, err)
				}
			}
			lineage, err := store.ExactDiffReviewerLineage(parentRun, parentManifest.Target, repositoryIdentity)
			if err != nil {
				return err
			}
			if err := webevidence.ValidateReviewerBindings(parentManifest.WebEvidence, lineage.Reviewers, lineage.SecurityReviews, lineage.LatestReviewers, lineage.LatestSecurityReviews); err != nil {
				return fmt.Errorf("run %s has incompatible reviewer web-evidence lineage: %w", parentRun.ID, err)
			}
			latestResults := append(append([]model.ReviewerResult(nil), lineage.LatestReviewers...), lineage.LatestSecurityReviews...)
			latestResults = append(latestResults, parentManifest.CrossExaminations...)
			selected, wholeReviewRetry, err := selectRetryReviewersForRun(parentManifest.WebEvidence != nil, latestResults, reviewers)
			if err != nil {
				return err
			}
			if wholeReviewRetry {
				fmt.Fprintln(command.ErrOrStderr(), "cora: web-backed retry will rerun every reviewer against the frozen snapshot")
			}
			// Legacy provider errors without an explicit zone describe the reset
			// in the provider CLI's local time, while manifests are stored in UTC.
			notBefore := quotaNotBefore(latestResults, selected, parentManifest.FinishedAt.In(time.Local), time.Now())
			if noWait {
				for _, retryAt := range notBefore {
					if retryAt.After(time.Now()) {
						return fmt.Errorf("selected provider is quota-limited until %s", retryAt.Local().Format(time.RFC3339))
					}
				}
			}

			personal, err := config.LoadPersonal()
			if err != nil {
				return err
			}
			reviewContext, trustedConfigTarget, err := retryAutoFixReviewContext(store, parentManifest)
			if err != nil {
				return err
			}
			cfg, err := loadTrustedConfig(ctx, repo, personal, trustedConfigTarget)
			if err != nil {
				return err
			}
			if parentManifest.ReviewPolicy == nil {
				return errors.New("saved run lacks an effective review-policy snapshot; start a fresh review instead of reusing unverifiable policy evidence")
			}
			cfg, err = config.ApplyReviewPolicy(cfg, *parentManifest.ReviewPolicy)
			if err != nil {
				return fmt.Errorf("restore saved review policy: %w", err)
			}
			applyReviewPolicyOverrides(&cfg, reviewPolicyOverrides{
				AllowAPIBilling: allowAPIBilling, Adjudicate: adjudicate, Strict: strict,
			})
			// Web records require a complete saved policy and rerun every enabled
			// role. Do not infer enablement from a prior synthetic/missing result.
			if parentManifest.WebEvidence == nil {
				preserveRetryReviewerSettings(&cfg, parentManifest, lineage, selected)
			}
			reviewerExecutionLimits := savedReviewerExecutionLimits(cfg, parentManifest)
			limitOverrides, err := applyRetryLimitOverrides(&cfg, reviewerExecutionLimits, selected, retryLimitOverrideInput{
				ReviewerTimeout: reviewerTimeout, ReviewerTimeoutSet: command.Flags().Changed("reviewer-timeout"),
				OverallTimeout: overallTimeout, OverallTimeoutSet: command.Flags().Changed("overall-timeout"),
				MaxTurns: maxTurns, MaxTurnsSet: command.Flags().Changed("max-turns"),
			})
			if err != nil {
				return err
			}
			if parentManifest.WebEvidence == nil && ((selected["codex"] && !cfg.Reviewers.Codex.Enabled) || (selectedAny(selected, "claude", "claude-security", "claude-escalation", "claude-cross-examination") && !cfg.Reviewers.Claude.Enabled)) {
				return errors.New("selected reviewer is disabled by the trusted configuration")
			}
			previous := prepareRetryResults(lineage.Reviewers, lineage.LatestReviewers, selected)
			previousSecurity := prepareRetryResults(lineage.SecurityReviews, lineage.LatestSecurityReviews, selected)
			previousCross := prepareRetryResults(parentManifest.CrossExaminations, parentManifest.CrossExaminations, selected)
			if parentManifest.WebEvidence != nil {
				// The orchestrator reads attempt counters from the validated parent.
				// Passing any prior result here would make it reusable evidence, which
				// web-backed retries categorically forbid.
				previous = nil
				previousSecurity = nil
				previousCross = nil
			}
			runOptions := orchestrator.RunOptions{
				ParentRunID: parentRun.ID, RetryLimitOverrides: limitOverrides, ReviewerExecutionLimits: reviewerExecutionLimits,
				RetryReviewers: selected, ReuseReviewers: previous, ReuseSecurityReviews: previousSecurity,
				ReuseCrossExaminations: previousCross,
				ReuseChecks:            true, Checks: parentManifest.Checks, NotBefore: notBefore,
				ReuseWebEvidence: parentManifest.WebEvidence,
				AutoFixLoopID:    parentManifest.AutoFixLoopID, AutoFixIteration: parentManifest.AutoFixIteration,
			}
			reviewRunner := runner(command)
			var decision model.Decision
			if reviewContext.ReviewScope != "" {
				decision, err = reviewRunner.RunAutoFixReview(ctx, repo, parentManifest.Target, cfg, runOptions, reviewContext)
			} else {
				decision, err = reviewRunner.RunWithOptions(ctx, repo, parentManifest.Target, cfg, runOptions)
			}
			if err != nil {
				return err
			}
			if opts.json {
				if err := printJSON(decision); err != nil {
					return err
				}
			} else {
				printDecision(decision)
			}
			if decision.State != model.StateApproved {
				return stateError{state: decision.State}
			}
			return nil
		},
	}
	command.Flags().StringSliceVar(&reviewers, "reviewer", nil, "reviewer to retry: codex, claude, claude-security, claude-escalation, or claude-cross-examination (repeatable)")
	command.Flags().BoolVar(&noWait, "no-wait", false, "return instead of waiting for a recorded provider quota reset")
	command.Flags().BoolVar(&allowAPIBilling, "allow-api-billing", false, "allow API-key or other separately billed authentication")
	command.Flags().BoolVar(&adjudicate, "adjudicate", false, "run a Fable adjudicator when reviewers disagree")
	command.Flags().BoolVar(&strict, "strict", false, "treat minor findings as blocking and require validation checks")
	command.Flags().DurationVar(&reviewerTimeout, "reviewer-timeout", 0, "raise the execution timeout for each selected reviewer")
	command.Flags().DurationVar(&overallTimeout, "overall-timeout", 0, "raise the retry's overall active execution timeout")
	command.Flags().IntVar(&maxTurns, "max-turns", 0, "raise the Claude turn ceiling for each selected Claude-backed reviewer")
	return command
}

func runner(command *cobra.Command) orchestrator.Runner {
	return orchestrator.Runner{Version: Version, SourceSHA: SourceSHA, BuildTime: BuildTime, Progress: command.ErrOrStderr()}
}

type retryLimitOverrideInput struct {
	ReviewerTimeout    time.Duration
	ReviewerTimeoutSet bool
	OverallTimeout     time.Duration
	OverallTimeoutSet  bool
	MaxTurns           int
	MaxTurnsSet        bool
}

// applyRetryLimitOverrides permits a retry to raise the limits that ended an
// earlier attempt without silently weakening or otherwise replacing the saved
// review policy. The resulting child manifest snapshots the per-role limits,
// the complete effective policy, and this explicit operator-requested delta.
func applyRetryLimitOverrides(cfg *config.Config, reviewerLimits map[string]model.ReviewerExecutionLimit, selected map[string]bool, input retryLimitOverrideInput) (*model.RetryLimitOverrides, error) {
	if cfg == nil {
		return nil, errors.New("retry configuration is unavailable")
	}
	if reviewerLimits == nil {
		return nil, errors.New("saved per-reviewer execution limits are unavailable")
	}
	if !input.ReviewerTimeoutSet && !input.OverallTimeoutSet && !input.MaxTurnsSet {
		return nil, nil
	}
	selectedNames := make([]string, 0, len(selected))
	for reviewer, enabled := range selected {
		if enabled {
			selectedNames = append(selectedNames, reviewer)
		}
	}
	sort.Strings(selectedNames)
	overrides := &model.RetryLimitOverrides{Reviewers: selectedNames}

	if input.OverallTimeoutSet {
		if input.OverallTimeout <= cfg.OverallTimeout.Duration {
			return nil, fmt.Errorf("--overall-timeout must raise the saved limit above %s", cfg.OverallTimeout.Duration)
		}
		cfg.OverallTimeout = config.Duration{Duration: input.OverallTimeout}
		value := model.NewDuration(input.OverallTimeout)
		overrides.OverallTimeout = &value
	}
	if input.ReviewerTimeoutSet {
		if input.ReviewerTimeout <= 0 {
			return nil, errors.New("--reviewer-timeout must be positive")
		}
		for _, reviewer := range selectedNames {
			limit, found := reviewerLimits[reviewer]
			if !found {
				return nil, fmt.Errorf("saved execution limits are missing reviewer %s", reviewer)
			}
			current := limit.Timeout.Duration
			if input.ReviewerTimeout <= current {
				return nil, fmt.Errorf("--reviewer-timeout must raise %s's saved limit above %s", reviewer, current)
			}
		}
		if input.ReviewerTimeout > cfg.OverallTimeout.Duration {
			return nil, fmt.Errorf("--reviewer-timeout %s exceeds the effective overall timeout %s; also raise --overall-timeout", input.ReviewerTimeout, cfg.OverallTimeout.Duration)
		}
		for _, reviewer := range selectedNames {
			limit := reviewerLimits[reviewer]
			limit.Timeout = model.NewDuration(input.ReviewerTimeout)
			reviewerLimits[reviewer] = limit
		}
		value := model.NewDuration(input.ReviewerTimeout)
		overrides.ReviewerTimeout = &value
	}
	if input.MaxTurnsSet {
		if !selectedAny(selected, "claude", "claude-security", "claude-escalation", "claude-cross-examination") {
			return nil, errors.New("--max-turns requires at least one selected Claude-backed reviewer")
		}
		if input.MaxTurns <= cfg.Reviewers.Claude.FinalizationTurns {
			return nil, fmt.Errorf("--max-turns must exceed the saved %d-turn finalization reserve", cfg.Reviewers.Claude.FinalizationTurns)
		}
		for _, reviewer := range selectedNames {
			if reviewer == "codex" {
				continue
			}
			limit, found := reviewerLimits[reviewer]
			if !found {
				return nil, fmt.Errorf("saved execution limits are missing reviewer %s", reviewer)
			}
			if input.MaxTurns <= limit.MaxTurns {
				return nil, fmt.Errorf("--max-turns must raise %s's saved limit above %d", reviewer, limit.MaxTurns)
			}
			limit.MaxTurns = input.MaxTurns
			reviewerLimits[reviewer] = limit
		}
		value := input.MaxTurns
		overrides.MaxTurns = &value
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate retry limit overrides: %w", err)
	}
	return overrides, nil
}

func savedReviewerExecutionLimits(cfg config.Config, manifest model.Manifest) map[string]model.ReviewerExecutionLimit {
	limits := config.SnapshotReviewerExecutionLimits(cfg)
	for reviewer, limit := range manifest.ReviewerExecutionLimits {
		limits[reviewer] = limit
	}
	return limits
}

func retryAutoFixReviewContext(store record.Store, manifest model.Manifest) (model.AutoFixReviewContext, model.Target, error) {
	trustedConfigTarget := manifest.Target
	reviewScope, err := webevidence.EffectiveReviewScope(manifest)
	if err != nil {
		return model.AutoFixReviewContext{}, model.Target{}, err
	}
	if reviewScope == "" || reviewScope == "full" && manifest.AutoFixLoopID == "" {
		return model.AutoFixReviewContext{}, trustedConfigTarget, nil
	}
	if manifest.AutoFixLoopID == "" {
		return model.AutoFixReviewContext{}, model.Target{}, errors.New("scoped review record is missing its auto-fix lineage")
	}
	if manifest.FullTarget == nil {
		return model.AutoFixReviewContext{}, model.Target{}, errors.New("scoped auto-fix review record is missing its complete target")
	}
	reviewContext := model.AutoFixReviewContext{
		ReviewScope: reviewScope, ApprovalBaselineRunID: manifest.ApprovalBaselineRunID,
		ApprovalBaselineHash: manifest.ApprovalBaselineHash, FullTarget: *manifest.FullTarget,
		TrustedBaseSHA: manifest.Target.BaseSHA,
	}
	if manifest.ApprovalBaselineRunID != "" {
		baselineRun, err := store.Resolve(manifest.ApprovalBaselineRunID)
		if err != nil {
			return model.AutoFixReviewContext{}, model.Target{}, fmt.Errorf("resolve approved retry baseline: %w", err)
		}
		baseline, err := record.LoadApprovedBaseline(baselineRun)
		if err != nil {
			return model.AutoFixReviewContext{}, model.Target{}, fmt.Errorf("validate approved retry baseline: %w", err)
		}
		if baseline.Decision.DiffHash != manifest.ApprovalBaselineHash {
			return model.AutoFixReviewContext{}, model.Target{}, errors.New("saved retry baseline hash does not match its approved run")
		}
		reviewContext.TrustedBaseSHA = baseline.Decision.BaseSHA
		reviewContext.BaselineFindings = append([]model.ConsolidatedFinding(nil), baseline.Decision.Findings...)
	} else if reviewScope == "approved-baseline-delta" {
		return model.AutoFixReviewContext{}, model.Target{}, errors.New("delta review record is missing its approved baseline")
	}
	trustedConfigTarget.BaseSHA = reviewContext.TrustedBaseSHA
	return reviewContext, trustedConfigTarget, nil
}

func selectRetryReviewers(results []model.ReviewerResult, requested []string) (map[string]bool, error) {
	available := make(map[string]model.ReviewerResult)
	for _, result := range results {
		if result.Reviewer == "codex" || result.Reviewer == "claude" || result.Reviewer == "claude-security" || result.Reviewer == "claude-escalation" || result.Reviewer == "claude-cross-examination" {
			available[result.Reviewer] = result
		}
	}
	selected := make(map[string]bool)
	if len(requested) == 0 {
		for name, result := range available {
			if !reusableReviewerResult(result) {
				selected[name] = true
			}
		}
		if len(selected) == 0 {
			return nil, errors.New("all required reviewers completed; pass --reviewer to rerun one explicitly")
		}
		return selected, nil
	}
	for _, name := range requested {
		name = strings.ToLower(strings.TrimSpace(name))
		if _, found := available[name]; !found {
			return nil, fmt.Errorf("reviewer %q is not present in the saved run", name)
		}
		selected[name] = true
	}
	return selected, nil
}

func selectRetryReviewersForRun(webBacked bool, results []model.ReviewerResult, requested []string) (map[string]bool, bool, error) {
	if !webBacked {
		selected, err := selectRetryReviewers(results, requested)
		return selected, false, err
	}
	if len(requested) > 0 {
		return nil, false, errors.New("web-backed retries rerun every reviewer against the frozen snapshot; omit --reviewer")
	}
	return allRetryReviewers(), true, nil
}

// Web-backed retries intentionally have no targeted mode. Every ordinary and
// conditional reviewer role must be eligible because rerunning an upstream
// reviewer can newly trigger security review, adjudication, or cross-examination.
func allRetryReviewers() map[string]bool {
	return map[string]bool{
		"codex":                    true,
		"claude":                   true,
		"claude-security":          true,
		"claude-escalation":        true,
		"claude-cross-examination": true,
	}
}

// reusableReviewerResult distinguishes a provider process that returned cleanly
// from review evidence that actually completed. Reserved-turn finalization can
// deliberately produce a valid completed abstention with incomplete context;
// that result remains fail-closed and must be selected by the default retry.
func reusableReviewerResult(result model.ReviewerResult) bool {
	if result.Status != "completed" || result.Report == nil || !result.Report.ContextComplete {
		return false
	}
	switch result.Report.Verdict {
	case "approve", "request_changes":
		return true
	default:
		return false
	}
}

func prepareRetryResults(preserved, latest []model.ReviewerResult, selected map[string]bool) []model.ReviewerResult {
	latestByName := make(map[string]model.ReviewerResult, len(latest))
	for _, result := range latest {
		latestByName[result.Reviewer] = result
	}
	results := append([]model.ReviewerResult(nil), preserved...)
	for index := range results {
		if !selected[results[index].Reviewer] {
			continue
		}
		if latestResult, found := latestByName[results[index].Reviewer]; found {
			results[index] = latestResult
		}
		results[index].Status = "incomplete"
		results[index].Report = nil
		results[index].ReusedFromRunID = ""
	}
	return results
}

func selectedAny(selected map[string]bool, reviewers ...string) bool {
	for _, reviewer := range reviewers {
		if selected[reviewer] {
			return true
		}
	}
	return false
}

func quotaNotBefore(results []model.ReviewerResult, selected map[string]bool, observedAt, now time.Time) map[string]time.Time {
	notBefore := make(map[string]time.Time)
	for _, result := range results {
		if !selected[result.Reviewer] {
			continue
		}
		retryAt := result.RetryAt
		if retryAt == nil {
			if parsed, quota := provider.QuotaRetryAt(result.Error, observedAt); quota && !parsed.IsZero() {
				retryAt = &parsed
			}
		}
		if retryAt != nil && retryAt.After(now) {
			notBefore[result.Reviewer] = *retryAt
		}
	}
	return notBefore
}

// preserveRetryReviewerSettings keeps each selected role on the same effective
// model and effort as its audited attempt. Specialized Fable phases also retain
// their independent saved limits when the parent recorded an effective policy.
func preserveRetryReviewerSettings(cfg *config.Config, manifest model.Manifest, lineage record.ReviewerLineage, selected map[string]bool) {
	if cfg == nil {
		return
	}
	if selected["codex"] {
		if previous, found := effectiveRetryResult("codex", lineage.Reviewers, lineage.LatestReviewers); found {
			cfg.Reviewers.Codex.Enabled = true
			applySavedModelEffort(&cfg.Reviewers.Codex.Model, &cfg.Reviewers.Codex.Effort, previous)
		}
	}
	if selected["claude"] {
		if previous, found := effectiveRetryResult("claude", lineage.Reviewers, lineage.LatestReviewers); found {
			cfg.Reviewers.Claude.Enabled = true
			applySavedModelEffort(&cfg.Reviewers.Claude.Model, &cfg.Reviewers.Claude.Effort, previous)
		}
	}

	targeted := []struct {
		name      string
		preserved []model.ReviewerResult
		latest    []model.ReviewerResult
	}{
		{name: "claude-security", preserved: lineage.SecurityReviews, latest: lineage.LatestSecurityReviews},
		{name: "claude-escalation", preserved: lineage.Reviewers, latest: lineage.LatestReviewers},
		{name: "claude-cross-examination", preserved: manifest.CrossExaminations, latest: manifest.CrossExaminations},
	}
	for _, role := range targeted {
		if !selected[role.name] {
			continue
		}
		previous, found := effectiveRetryResult(role.name, role.preserved, role.latest)
		if !found {
			continue
		}
		cfg.Reviewers.Claude.Enabled = true
		cfg.Escalation.Enabled = true
		applySavedModelEffort(&cfg.Escalation.Model, &cfg.Escalation.Effort, previous)
		switch role.name {
		case "claude-security":
			cfg.Escalation.ForceSecuritySensitive = true
		case "claude-escalation":
			cfg.Escalation.AdjudicateDisagreements = true
		case "claude-cross-examination":
			cfg.CrossExamineBlockingFindings = true
		}
	}
	if manifest.ReviewPolicy != nil && selectedAny(selected, "claude-security", "claude-escalation", "claude-cross-examination") {
		policy := manifest.ReviewPolicy
		cfg.Escalation.MaxTurns = cloneIntPointer(policy.Escalation.MaxTurns)
		cfg.Escalation.MaxBudgetUSD = cloneFloatPointer(policy.Escalation.MaxBudgetUSD)
		if selected["claude-cross-examination"] {
			cfg.CrossExamination.Timeout = config.Duration{Duration: policy.CrossExamination.Timeout.Duration}
			cfg.CrossExamination.MaxTurns = policy.CrossExamination.MaxTurns
			cfg.CrossExamination.MaxBudgetUSD = policy.CrossExamination.MaxBudgetUSD
		}
	}
}

func applySavedModelEffort(modelName, effort *string, result model.ReviewerResult) {
	if result.Model != "" {
		*modelName = result.Model
	}
	if result.Effort != "" {
		*effort = result.Effort
	}
}

func cloneIntPointer(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneFloatPointer(value *float64) *float64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func effectiveRetryResult(name string, preserved, latest []model.ReviewerResult) (model.ReviewerResult, bool) {
	var result model.ReviewerResult
	found := false
	for _, candidate := range preserved {
		if candidate.Reviewer == name {
			result, found = candidate, true
			break
		}
	}
	for _, candidate := range latest {
		if candidate.Reviewer != name {
			continue
		}
		if !found {
			result, found = candidate, true
			break
		}
		if candidate.Model != "" {
			result.Model = candidate.Model
		}
		if candidate.Effort != "" {
			result.Effort = candidate.Effort
		}
		if candidate.EscalationCause != "" {
			result.EscalationCause = candidate.EscalationCause
		}
		break
	}
	return result, found
}

func expandAutoProfiles(ctx context.Context, repo gitx.Repo, target model.Target, names []string) ([]string, error) {
	expanded := make([]string, 0, len(names))
	seen := make(map[string]bool)
	for _, name := range names {
		if name != "auto" {
			if !seen[name] {
				expanded = append(expanded, name)
				seen[name] = true
			}
			continue
		}
		markers := []struct {
			profile string
			paths   []string
		}{
			{profile: "go", paths: []string{"go.mod"}},
			{profile: "node", paths: []string{"package.json"}},
			{profile: "python", paths: []string{"pyproject.toml", "pytest.ini", "setup.cfg"}},
		}
		for _, marker := range markers {
			if seen[marker.profile] {
				continue
			}
			for _, path := range marker.paths {
				_, found, err := repo.ReadFileAt(ctx, target.HeadSHA, path)
				if err != nil {
					return nil, err
				}
				if found {
					expanded = append(expanded, marker.profile)
					seen[marker.profile] = true
					break
				}
			}
		}
	}
	return expanded, nil
}

func newStatusCommand(opts *options) *cobra.Command {
	var active bool
	command := &cobra.Command{
		Use:   "status",
		Short: "Show the latest review run",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			repo, err := gitx.Discover(command.Context(), opts.repo)
			if err != nil {
				return err
			}
			store := record.New(repo.CommonDir)
			if active {
				summaries, err := loadRunSummaries(store, 0)
				if err != nil {
					return err
				}
				autoFixSummaries, err := loadAutoFixSummaries(store)
				if err != nil {
					return err
				}
				summaries = append(summaries, autoFixSummaries...)
				activeRuns := make([]model.RunSummary, 0)
				for _, summary := range summaries {
					if showInActiveStatus(summary) {
						activeRuns = append(activeRuns, summary)
					}
				}
				sort.Slice(activeRuns, func(i, j int) bool { return activeRuns[i].StartedAt.After(activeRuns[j].StartedAt) })
				if opts.json {
					return printJSON(activeRuns)
				}
				if len(activeRuns) == 0 {
					fmt.Fprintln(command.OutOrStdout(), "No active CORA runs.")
					return nil
				}
				printActiveRuns(command.OutOrStdout(), activeRuns)
				return nil
			}
			run, err := store.Resolve("latest")
			if err != nil {
				return err
			}
			decision, decisionErr := record.LoadDecision(run)
			if decisionErr != nil {
				summary, summaryErr := loadRunSummary(run)
				if summaryErr != nil {
					return decisionErr
				}
				if opts.json {
					return printJSON(summary)
				}
				printRunSummary(summary)
				return nil
			}
			manifest, manifestErr := record.LoadManifest(run)
			if manifestErr != nil {
				return manifestErr
			}
			if err := validateWebRecordForDisplay(run, manifest); err != nil {
				return err
			}
			reviewScope, scopeErr := webevidence.EffectiveReviewScope(manifest)
			if scopeErr != nil {
				return scopeErr
			}
			displayDecision := decisionForDisplay(decision, reviewScope)
			if opts.json {
				return printJSON(displayDecision)
			}
			printDecision(displayDecision)
			return nil
		},
	}
	command.Flags().BoolVar(&active, "active", false, "show all currently active runs")
	return command
}

func showInActiveStatus(summary model.RunSummary) bool {
	switch summary.State {
	case "active", "quota-queued", model.StatePaused:
		return true
	}
	if summary.State == "interrupted" {
		for _, state := range summary.Reviewers {
			if state == "quota-queued" {
				return true
			}
		}
	}
	return false
}

func newListCommand(opts *options) *cobra.Command {
	var state string
	var head string
	var limit int
	command := &cobra.Command{
		Use:   "list",
		Short: "List saved review runs",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			repo, err := gitx.Discover(command.Context(), opts.repo)
			if err != nil {
				return err
			}
			summaries, err := loadRunSummaries(record.New(repo.CommonDir), 0)
			if err != nil {
				return err
			}
			filtered := make([]model.RunSummary, 0, len(summaries))
			for _, summary := range summaries {
				if state != "" && summary.State != state {
					continue
				}
				if head != "" && !strings.HasPrefix(summary.HeadSHA, head) {
					continue
				}
				filtered = append(filtered, summary)
				if limit > 0 && len(filtered) >= limit {
					break
				}
			}
			if opts.json {
				return printJSON(filtered)
			}
			if len(filtered) == 0 {
				fmt.Fprintln(command.OutOrStdout(), "No CORA runs found.")
				return nil
			}
			fmt.Fprintln(command.OutOrStdout(), "RUN                                      STATE               HEAD      WALL      ACTIVE    PARENT")
			for _, summary := range filtered {
				parent := summary.ParentRunID
				if parent == "" && summary.AutoFixLoopID != "" {
					parent = fmt.Sprintf("%s#%d", summary.AutoFixLoopID, summary.AutoFixIteration)
				}
				if parent == "" {
					parent = "-"
				}
				fmt.Fprintf(command.OutOrStdout(), "%-40s %-19s %-9s %-9s %-9s %s\n", summary.RunID, summary.State, shortSHA(summary.HeadSHA), formatMilliseconds(summary.ElapsedMS), formatActiveExecution(summary), parent)
			}
			return nil
		},
	}
	command.Flags().StringVar(&state, "state", "", "filter by run state")
	command.Flags().StringVar(&head, "head", "", "filter by head SHA prefix")
	command.Flags().IntVar(&limit, "limit", 20, "maximum runs to show (0 means all)")
	return command
}

func loadRunSummaries(store record.Store, limit int) ([]model.RunSummary, error) {
	runs, err := store.Runs()
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(runs) > limit {
		runs = runs[:limit]
	}
	summaries := make([]model.RunSummary, 0, len(runs))
	for _, run := range runs {
		summary, err := loadRunSummary(run)
		if err == nil {
			summaries = append(summaries, summary)
		}
	}
	return summaries, nil
}

type autoFixHeartbeat struct {
	State     string    `json:"state"`
	Phase     string    `json:"phase"`
	Iteration int       `json:"iteration"`
	UpdatedAt time.Time `json:"updated_at"`
}

func loadAutoFixSummaries(store record.Store) ([]model.RunSummary, error) {
	loops, err := store.AutoFixLoops()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	summaries := make([]model.RunSummary, 0, len(loops))
	for _, run := range loops {
		loop, loadErr := record.LoadAutoFixLoop(run)
		if loadErr != nil || loop.State != "active" && loop.State != model.StatePaused {
			continue
		}
		wallElapsed := nonNegativeMilliseconds(now.Sub(loop.StartedAt))
		activeElapsed := now.Sub(loop.StartedAt) - loop.PausedDuration.Duration
		if loop.PausedAt != nil {
			activeElapsed -= now.Sub(loop.PausedAt.UTC())
		}
		if activeElapsed < 0 {
			activeElapsed = 0
		}
		summary := model.RunSummary{
			RunID: run.ID, State: loop.State, StartedAt: loop.StartedAt, ElapsedMS: wallElapsed,
			ActiveExecutionMS: activeElapsed.Milliseconds(), ActiveTimingBasis: "auto-fix-active-excludes-paused-quota",
			HeadSHA: loop.InitialHeadSHA, AutoFixLoopID: loop.LoopID, RepositoryIdentity: loop.RepositoryIdentity,
			Reviewers: make(map[string]string), Queues: make(map[string]model.ProviderQueueStatus), RecordPath: run.Path,
		}
		var heartbeat autoFixHeartbeat
		if record.ReadJSON(filepath.Join(run.Path, "heartbeat.json"), &heartbeat) == nil {
			summary.Phase = heartbeat.Phase
			if loop.State == "active" && now.Sub(heartbeat.UpdatedAt) > 2*heartbeatFreshnessWindow() {
				summary.State = "interrupted"
			}
		}
		if loop.State == model.StatePaused {
			summary.AwaitingResume = true
			resetPending := false
			if loop.RetryAt != nil {
				retryAt := *loop.RetryAt
				summary.RetryAt = &retryAt
				resetPending = retryAt.After(now)
			}
			if resetPending {
				summary.Phase = "paused-" + loop.ResumePhase
				for _, reviewer := range loop.ResumeReviewers {
					summary.Reviewers[reviewer] = "quota-queued"
					retryAt := *summary.RetryAt
					summary.Queues[reviewer] = model.ProviderQueueStatus{Provider: reviewer, ETAAt: &retryAt}
				}
			} else {
				summary.Phase = "awaiting-resume"
				summary.RetryReady = true
				for _, reviewer := range loop.ResumeReviewers {
					summary.Reviewers[reviewer] = "retry-ready"
				}
			}
		} else if summary.Phase != "" {
			summary.Reviewers["auto-fix"] = summary.Phase
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

func loadRunSummary(run record.Run) (model.RunSummary, error) {
	manifest, err := record.LoadManifest(run)
	if err != nil {
		return model.RunSummary{}, err
	}
	if err := validateWebRecordForDisplay(run, manifest); err != nil {
		return model.RunSummary{}, err
	}
	now := time.Now()
	finished := manifest.FinishedAt
	end := now
	if !finished.IsZero() {
		end = finished
	}
	summary := model.RunSummary{
		RunID: run.ID, State: "incomplete", StartedAt: manifest.StartedAt, FinishedAt: finished,
		ElapsedMS: end.Sub(manifest.StartedAt).Milliseconds(), HeadSHA: manifest.Target.HeadSHA,
		ActiveExecutionMS: manifest.ActiveExecution.Milliseconds(), ActiveTimingBasis: manifest.ActiveTimingBasis,
		ParentRunID: manifest.ParentRunID, AutoFixLoopID: manifest.AutoFixLoopID, AutoFixIteration: manifest.AutoFixIteration,
		RepositoryIdentity: manifest.RepositoryIdentity, RecordPath: run.Path,
	}
	if decision, decisionErr := record.LoadDecision(run); decisionErr == nil {
		reviewScope, scopeErr := webevidence.EffectiveReviewScope(manifest)
		if scopeErr != nil {
			return model.RunSummary{}, scopeErr
		}
		summary.State = decisionForDisplay(decision, reviewScope).State
		if reviewScope == "approved-baseline-delta" {
			summary.Phase = "final-full-review-required"
		}
		summary.Reviewers = decision.Reviewers
		summary.Checks = decision.Checks
		return summary, nil
	}
	if heartbeat, heartbeatErr := record.LoadHeartbeat(run); heartbeatErr == nil {
		summary.State = heartbeat.State
		summary.Phase = heartbeat.Phase
		summary.Reviewers = heartbeat.Reviewers
		summary.ReviewerVerdicts = heartbeat.ReviewerVerdicts
		summary.Checks = heartbeat.Checks
		summary.Queues = heartbeat.Queues
		if heartbeat.WallElapsed.Duration > 0 || heartbeat.State == "active" {
			summary.ElapsedMS = heartbeat.WallElapsed.Milliseconds()
		}
		summary.ActiveExecutionMS = heartbeat.ActiveExecution.Milliseconds()
		summary.ActiveTimingBasis = heartbeat.ActiveTimingBasis
		summary.ReviewerElapsedMS = make(map[string]int64)
		for name, state := range heartbeat.Reviewers {
			started := heartbeat.ReviewerStartedAt[name]
			if state == "running" && !started.IsZero() {
				summary.ReviewerElapsedMS[name] = nonNegativeMilliseconds(now.UTC().Sub(started.UTC()))
			}
		}
		if heartbeat.State == "active" && now.Sub(heartbeat.UpdatedAt) > 2*heartbeatFreshnessWindow() {
			summary.State = "interrupted"
		}
		if heartbeat.State != "active" && manifest.FinishedAt.IsZero() {
			summary.ElapsedMS = heartbeat.UpdatedAt.Sub(manifest.StartedAt).Milliseconds()
		}
	}
	return summary, nil
}

func heartbeatFreshnessWindow() time.Duration { return 30 * time.Second }

func printRunSummary(summary model.RunSummary) {
	fmt.Printf("%s %s\n", strings.ToUpper(summary.State), summary.RunID)
	if summary.Phase != "" {
		fmt.Printf("Phase: %s\n", summary.Phase)
	}
	fmt.Printf("Head: %s\nWall elapsed: %s\nActive execution: %s\nRecord: %s\n", shortSHA(summary.HeadSHA), formatMilliseconds(summary.ElapsedMS), formatActiveExecution(summary), summary.RecordPath)
	for _, name := range sortedStateNames(summary.Reviewers) {
		rawState := summary.Reviewers[name]
		state := rawState
		if verdict := summary.ReviewerVerdicts[name]; verdict != "" {
			state += " verdict=" + verdict
		}
		if elapsed := summary.ReviewerElapsedMS[name]; rawState == "running" && elapsed > 0 {
			state += " for " + formatMilliseconds(elapsed) + " wall"
		}
		fmt.Printf("%-18s %s\n", name+":", state)
	}
	for _, name := range sortedStateNames(summary.Checks) {
		fmt.Printf("%-18s %s\n", "check "+name+":", summary.Checks[name])
	}
	queueNames := make([]string, 0, len(summary.Queues))
	for name := range summary.Queues {
		queueNames = append(queueNames, name)
	}
	sort.Strings(queueNames)
	for _, name := range queueNames {
		queue := summary.Queues[name]
		fmt.Printf("%-18s position=%d ahead=%d active=%d/%d %s\n", "queue "+name+":", queue.Position, queue.Ahead, queue.Active, queue.Limit, formatQueueWait(queue, time.Now()))
	}
}

func printActiveRuns(writer io.Writer, summaries []model.RunSummary) {
	fmt.Fprintln(writer, "RUN                                      STATE          HEAD      WALL      ACTIVE    PHASE       REVIEWERS")
	for _, summary := range summaries {
		fmt.Fprintf(writer, "%-40s %-14s %-9s %-9s %-9s %-11s %s\n", summary.RunID, summary.State, shortSHA(summary.HeadSHA), formatMilliseconds(summary.ElapsedMS), formatActiveExecution(summary), summary.Phase, activeReviewerSummary(summary))
	}
}

func activeReviewerSummary(summary model.RunSummary) string {
	parts := make([]string, 0, len(summary.Reviewers))
	for _, name := range sortedStateNames(summary.Reviewers) {
		rawState := summary.Reviewers[name]
		state := rawState
		if verdict := summary.ReviewerVerdicts[name]; verdict != "" {
			state += "(verdict=" + verdict + ")"
		}
		if elapsed := summary.ReviewerElapsedMS[name]; rawState == "running" && elapsed > 0 {
			state += "(wall=" + formatMilliseconds(elapsed) + ")"
		}
		if queue, found := summary.Queues[name]; found {
			state = fmt.Sprintf("queued#%d", queue.Position)
			state += "(" + formatQueueWait(queue, time.Now()) + ")"
		}
		parts = append(parts, name+"="+state)
	}
	return strings.Join(parts, " ")
}

func sortedStateNames(states map[string]string) []string {
	names := make([]string, 0, len(states))
	for name := range states {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func formatMilliseconds(milliseconds int64) string {
	return (time.Duration(milliseconds) * time.Millisecond).Round(100 * time.Millisecond).String()
}

func nonNegativeMilliseconds(duration time.Duration) int64 {
	if duration < 0 {
		return 0
	}
	return duration.Milliseconds()
}

func formatActiveExecution(summary model.RunSummary) string {
	if summary.ActiveTimingBasis == "" {
		return "unknown"
	}
	return "~" + formatMilliseconds(summary.ActiveExecutionMS)
}

func formatQueueETA(etaAt, now time.Time) string {
	remaining := etaAt.Sub(now)
	if remaining <= 0 {
		return "waiting-for-capacity"
	}
	if remaining < time.Second {
		return "<1s"
	}
	return remaining.Round(time.Second).String()
}

func formatQueueWait(status model.ProviderQueueStatus, now time.Time) string {
	parts := make([]string, 0, 1+len(status.Holders))
	if status.ETAAt != nil && status.ETAAt.After(now) {
		parts = append(parts, "eta_in="+formatQueueETA(*status.ETAAt, now))
	}
	for _, holder := range status.Holders {
		identity := holder.Reviewer
		if identity == "" {
			identity = fmt.Sprintf("pid-%d", holder.PID)
		}
		if holder.RunID != "" {
			identity += "@" + holder.RunID
		}
		timeout := "timeout=unknown"
		if holder.TimeoutAt != nil {
			remaining := holder.TimeoutAt.Sub(now)
			if remaining > 0 {
				timeout = "timeout_in=" + formatMilliseconds(remaining.Milliseconds())
			} else {
				timeout = "timeout_overdue=" + formatMilliseconds((-remaining).Milliseconds())
			}
		}
		parts = append(parts, "holder="+identity+" "+timeout)
	}
	if len(parts) == 0 {
		return "waiting-for-capacity"
	}
	return strings.Join(parts, " ")
}

func newShowCommand(opts *options) *cobra.Command {
	var verbose bool
	command := &cobra.Command{
		Use:   "show [run-id]",
		Short: "Show a saved review run",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			repo, err := gitx.Discover(command.Context(), opts.repo)
			if err != nil {
				return err
			}
			id := "latest"
			if len(args) == 1 {
				id = args[0]
			}
			store := record.New(repo.CommonDir)
			run, err := store.Resolve(id)
			if err != nil {
				return err
			}
			manifest, err := record.LoadManifest(run)
			if err != nil {
				return err
			}
			decision, err := record.LoadDecision(run)
			if err != nil {
				return err
			}
			if err := validateWebRecordForDisplay(run, manifest); err != nil {
				return err
			}
			reviewScope, err := webevidence.EffectiveReviewScope(manifest)
			if err != nil {
				return err
			}
			displayDecision := decisionForDisplay(decision, reviewScope)
			view := struct {
				Manifest             model.Manifest `json:"manifest"`
				Decision             model.Decision `json:"decision"`
				EffectiveReviewScope string         `json:"effective_review_scope"`
			}{Manifest: manifest, Decision: displayDecision, EffectiveReviewScope: reviewScope}
			if opts.json {
				return printJSON(view)
			}
			printDecision(displayDecision)
			fmt.Printf("Started:    %s\n", manifest.StartedAt.Local().Format(time.RFC3339))
			fmt.Printf("Finished:   %s\n", manifest.FinishedAt.Local().Format(time.RFC3339))
			if manifest.ActiveTimingBasis != "" {
				fmt.Printf("Timing:     wall=%s active-execution=~%s (%s)\n", formatMilliseconds(manifest.WallElapsed.Milliseconds()), formatMilliseconds(manifest.ActiveExecution.Milliseconds()), manifest.ActiveTimingBasis)
			}
			if manifest.AutoFixLoopID != "" {
				fmt.Printf("Auto-fix:   %s iteration %d (scope=%s)\n", manifest.AutoFixLoopID, manifest.AutoFixIteration, reviewScope)
			}
			printConsolidatedDetails(command.OutOrStdout(), decision)
			allReviewers := manifestReviewerResults(manifest)
			for _, reviewer := range allReviewers {
				modelName := reviewer.Model
				if modelName == "" {
					modelName = "unknown"
				}
				effort := reviewer.Effort
				if effort == "" {
					effort = "default"
				}
				if reviewer.Report != nil {
					fmt.Printf("\n%s: %s (model=%s effort=%s)\n%s\n", strings.ToUpper(reviewer.Reviewer), reviewer.Report.Verdict, modelName, effort, reviewer.Report.Summary)
					if verbose {
						for _, finding := range reviewer.Report.Findings {
							fmt.Printf("  [%s] %s:%d %s\n", finding.Severity, finding.File, finding.Line, finding.Claim)
							fmt.Printf("    Confidence: %.0f%%\n", finding.Confidence*100)
							fmt.Printf("    Evidence: %s\n", finding.Evidence)
							fmt.Printf("    Suggested fix: %s\n", finding.SuggestedFix)
						}
						if len(reviewer.Report.OmittedPaths) > 0 {
							fmt.Printf("Omitted paths: %s\n", strings.Join(reviewer.Report.OmittedPaths, ", "))
						}
						if len(reviewer.Report.ResidualRisks) > 0 {
							fmt.Println("Residual risks:")
							for _, risk := range reviewer.Report.ResidualRisks {
								fmt.Printf("  - %s\n", risk)
							}
						}
					}
				} else {
					fmt.Printf("\n%s: incomplete (model=%s effort=%s) — %s\n", strings.ToUpper(reviewer.Reviewer), modelName, effort, reviewer.Error)
				}
				if reviewer.EscalationCause != "" {
					fmt.Printf("Escalation: %s\n", reviewer.EscalationCause)
				}
				fmt.Printf("Duration: wall-total=%s queue-wall=%s active-execution=~%s\n", formatMilliseconds(reviewer.Duration.Milliseconds()), formatMilliseconds(reviewer.QueueDuration.Milliseconds()), formatMilliseconds(reviewer.ExecutionDuration.Milliseconds()))
				fmt.Printf("Usage: %s\n", formatUsage(reviewer.Usage))
			}
			if len(manifest.Checks) > 0 {
				fmt.Println("\nChecks:")
				for _, check := range manifest.Checks {
					fmt.Printf("  %s: %s", check.Name, check.Status)
					if check.Error != "" {
						fmt.Printf(" — %s", check.Error)
					}
					fmt.Println()
					if evidence := check.ImportedEvidence; evidence != nil {
						fmt.Printf("    Imported evidence: verifier=%s source=%s verified=%s trust=%s\n", evidence.Verifier, evidence.Source, evidence.VerifiedAt.Format(time.RFC3339), evidence.Trust)
						fmt.Printf("    Artifact: %s sha256=%s\n", evidence.RecordFile, evidence.ContentSHA256)
						fmt.Printf("    Command (recorded, not executed): %s\n", strings.Join(evidence.Command, " "))
						fmt.Printf("    Summary: %s\n", evidence.Summary)
					}
				}
			}
			if evidence := manifest.WebEvidence; evidence != nil {
				fmt.Printf("\nWeb evidence: %s (%d source(s), snapshot-sha256=%s)\n", evidence.Mode, evidence.Count, evidence.SnapshotSHA256)
				if evidence.SourceRunID != "" {
					fmt.Printf("  Replayed from run: %s\n", evidence.SourceRunID)
				}
				fmt.Printf("  Index: %s sha256=%s\n", evidence.IndexFile, evidence.IndexSHA256)
				fmt.Printf("  Prompt: %s sha256=%s\n", evidence.PromptFile, evidence.PromptSHA256)
				for _, source := range evidence.Sources {
					fmt.Printf("  %s: %s (fetched=%s type=%s bytes=%d truncated=%t)\n",
						source.ID, source.FinalURL, source.FetchedAt.Format(time.RFC3339), source.ContentType, source.BodyBytes, source.Truncated)
					fmt.Printf("    Artifact: %s sha256=%s\n", source.BodyFile, source.BodySHA256)
				}
			}
			return nil
		},
	}
	command.Flags().BoolVarP(&verbose, "verbose", "v", false, "also show each original reviewer finding, omitted path, and residual risk")
	return command
}

func validateWebRecordForDisplay(run record.Run, manifest model.Manifest) error {
	if !record.IsWebEvidenceRun(run) && manifest.WebEvidence == nil {
		return nil
	}
	store := record.Store{Root: filepath.Dir(filepath.Dir(run.Path))}
	if err := store.ValidateReviewArtifacts(run, manifest); err != nil {
		return fmt.Errorf("run %s has invalid immutable review inputs: %w", run.ID, err)
	}
	return nil
}

func manifestReviewerResults(manifest model.Manifest) []model.ReviewerResult {
	results := append([]model.ReviewerResult(nil), manifest.Reviewers...)
	results = append(results, manifest.SecurityReviews...)
	results = append(results, manifest.CrossExaminations...)
	return results
}

func printConsolidatedDetails(writer io.Writer, decision model.Decision) {
	crossByFinding := make(map[string]model.CrossExamination, len(decision.CrossExaminations))
	for _, examination := range decision.CrossExaminations {
		crossByFinding[examination.FindingID] = examination
	}
	if len(decision.Findings) > 0 {
		fmt.Fprintln(writer, "\nConsolidated findings:")
		for _, finding := range decision.Findings {
			fmt.Fprintf(writer, "  [%s] %s:%d %s (%s)\n", finding.Severity, finding.File, finding.Line, finding.Claim, strings.Join(finding.Reviewers, ", "))
			fmt.Fprintf(writer, "    Confidence: %.0f%%\n", finding.Confidence*100)
			if len(finding.CarriedFromRunIDs) > 0 {
				fmt.Fprintf(writer, "    Carried from runs: %s\n", strings.Join(finding.CarriedFromRunIDs, ", "))
			}
			for _, evidence := range finding.Evidence {
				fmt.Fprintf(writer, "    Evidence: %s\n", evidence)
			}
			for _, fix := range finding.SuggestedFixes {
				fmt.Fprintf(writer, "    Suggested fix: %s\n", fix)
			}
			if examination, found := crossByFinding[finding.ID]; found {
				printCrossExaminationDetails(writer, examination)
			} else {
				printReachabilityDetails(writer, finding.Reachability, "    ")
			}
		}
	}
	if len(decision.RejectedFindings) > 0 {
		fmt.Fprintln(writer, "\nDisproved findings:")
		for _, finding := range decision.RejectedFindings {
			fmt.Fprintf(writer, "  [%s] %s:%d %s\n", finding.OriginalSeverity, finding.File, finding.Line, finding.Claim)
			fmt.Fprintf(writer, "    Confidence: %.0f%%\n", finding.Confidence*100)
			if len(finding.CarriedFromRunIDs) > 0 {
				fmt.Fprintf(writer, "    Carried from runs: %s\n", strings.Join(finding.CarriedFromRunIDs, ", "))
			}
			for _, evidence := range finding.Evidence {
				fmt.Fprintf(writer, "    Original evidence: %s\n", evidence)
			}
			if examination, found := crossByFinding[finding.ID]; found {
				printCrossExaminationDetails(writer, examination)
			}
		}
	}
	if len(decision.ResidualRisks) > 0 {
		fmt.Fprintln(writer, "\nResidual risks:")
		for _, risk := range decision.ResidualRisks {
			fmt.Fprintf(writer, "  - %s\n", risk)
		}
	}
}

func printCrossExaminationDetails(writer io.Writer, examination model.CrossExamination) {
	fmt.Fprintf(writer, "    Cross-examination: %s by %s (%s -> %s)\n", examination.Disposition, examination.Reviewer, examination.OriginalSeverity, examination.EffectiveSeverity)
	if examination.Rationale != "" {
		fmt.Fprintf(writer, "    Rationale: %s\n", examination.Rationale)
	}
	if examination.Reachability == nil {
		return
	}
	printReachabilityDetails(writer, examination.Reachability, "    ")
}

func printReachabilityDetails(writer io.Writer, reachability *model.Reachability, indent string) {
	if reachability == nil {
		return
	}
	fmt.Fprintf(writer, "%sReachability: %s\n", indent, reachability.Status)
	if reachability.Trigger != "" {
		fmt.Fprintf(writer, "%s  Trigger: %s\n", indent, reachability.Trigger)
	}
	if len(reachability.Path) > 0 {
		fmt.Fprintf(writer, "%s  Path: %s\n", indent, strings.Join(reachability.Path, " -> "))
	}
	if reachability.Impact != "" {
		fmt.Fprintf(writer, "%s  Impact: %s\n", indent, reachability.Impact)
	}
	if len(reachability.Preconditions) > 0 {
		fmt.Fprintf(writer, "%s  Preconditions: %s\n", indent, strings.Join(reachability.Preconditions, ", "))
	}
}

func newVerifyCommand(opts *options) *cobra.Command {
	var head string
	var runID string
	command := &cobra.Command{
		Use:   "verify",
		Short: "Verify that an approval matches a Git commit",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			ctx := command.Context()
			repo, err := gitx.Discover(ctx, opts.repo)
			if err != nil {
				return err
			}
			repositoryIdentity, err := repo.StableIdentity(ctx)
			if err != nil {
				return err
			}
			resolvedHead, err := repo.ResolveRevision(ctx, head)
			if err != nil {
				return err
			}
			store := record.New(repo.CommonDir)
			run, decision, manifest, err := findApproval(store, runID, resolvedHead, repositoryIdentity)
			if err != nil {
				if opts.json {
					_ = printJSON(map[string]any{"state": model.StateStale, "head_sha": resolvedHead, "error": err.Error()})
				} else {
					fmt.Fprintf(os.Stderr, "STALE %s\nReason: %s\n", shortSHA(resolvedHead), err)
				}
				return stateError{state: model.StateStale}
			}
			valid, err := repo.VerifyTarget(ctx, manifest.Target)
			if err != nil {
				return err
			}
			if !valid || decision.HeadSHA != resolvedHead || decision.DiffHash != manifest.Target.DiffHash {
				if opts.json {
					_ = printJSON(map[string]any{"state": model.StateStale, "run_id": run.ID, "head_sha": resolvedHead})
				} else {
					fmt.Fprintf(os.Stderr, "STALE %s\nReason: saved approval does not match the requested Git change\n", shortSHA(resolvedHead))
				}
				return stateError{state: model.StateStale}
			}
			output := map[string]any{"state": model.StateApproved, "run_id": run.ID, "head_sha": resolvedHead, "record_path": run.Path}
			if opts.json {
				return printJSON(output)
			}
			fmt.Printf("APPROVED %s\nRun: %s\nRecord: %s\n", shortSHA(resolvedHead), run.ID, run.Path)
			return nil
		},
	}
	command.Flags().StringVar(&head, "head", "HEAD", "commit to verify")
	command.Flags().StringVar(&runID, "run", "", "specific run ID")
	return command
}

func newCompletionCommand(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:       "completion [bash|zsh|fish|powershell]",
		Short:     "Generate shell completion",
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		Args:      cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return root.GenBashCompletion(os.Stdout)
			case "zsh":
				return root.GenZshCompletion(os.Stdout)
			case "fish":
				return root.GenFishCompletion(os.Stdout, true)
			case "powershell":
				return root.GenPowerShellCompletion(os.Stdout)
			default:
				return errors.New("unsupported shell")
			}
		},
	}
}

func loadTrustedConfig(ctx context.Context, repo gitx.Repo, personal config.Config, target model.Target) (config.Config, error) {
	contents, found, err := repo.ReadFileAt(ctx, target.BaseSHA, ".cora/config.toml")
	if err != nil {
		return config.Config{}, fmt.Errorf("read trusted repository config: %w", err)
	}
	if !found {
		return config.ApplyRepository(personal, "", nil)
	}
	source := fmt.Sprintf("git:%s:.cora/config.toml", target.BaseSHA)
	return config.ApplyRepository(personal, source, contents)
}

func findApproval(store record.Store, runID, headSHA, repositoryIdentity string) (record.Run, model.Decision, model.Manifest, error) {
	var runs []record.Run
	var err error
	if runID != "" {
		run, resolveErr := store.Resolve(runID)
		if resolveErr != nil {
			return record.Run{}, model.Decision{}, model.Manifest{}, resolveErr
		}
		runs = []record.Run{run}
	} else {
		runs, err = store.Runs()
		if err != nil {
			return record.Run{}, model.Decision{}, model.Manifest{}, err
		}
	}
	for _, run := range runs {
		decision, err := record.LoadDecision(run)
		if err != nil || decision.State != model.StateApproved || decision.HeadSHA != headSHA {
			continue
		}
		manifest, manifestErr := record.LoadManifest(run)
		if manifestErr != nil {
			continue
		}
		reviewScope, scopeErr := webevidence.EffectiveReviewScope(manifest)
		if scopeErr != nil || reviewScope == "approved-baseline-delta" || manifest.FinishedAt.IsZero() ||
			manifest.Target.BaseSHA != decision.BaseSHA || manifest.Target.HeadSHA != decision.HeadSHA ||
			manifest.Target.DiffHash != decision.DiffHash || !manifestChecksPassed(manifest.Checks) {
			continue
		}
		if manifest.RepositoryIdentity == "" || manifest.RepositoryIdentity != repositoryIdentity {
			if runID != "" {
				return record.Run{}, model.Decision{}, model.Manifest{}, fmt.Errorf("run %s belongs to a different repository identity", run.ID)
			}
			continue
		}
		if record.IsWebEvidenceRun(run) || manifest.WebEvidence != nil {
			if artifactErr := store.ValidateReviewArtifacts(run, manifest); artifactErr != nil {
				if runID != "" {
					return record.Run{}, model.Decision{}, model.Manifest{}, fmt.Errorf("run %s has invalid immutable review inputs: %w", run.ID, artifactErr)
				}
				continue
			}
		} else {
			if evidenceErr := record.ValidateImportedValidationEvidence(run, manifest.Target, repositoryIdentity, manifest.Checks); evidenceErr != nil {
				if runID != "" {
					return record.Run{}, model.Decision{}, model.Manifest{}, fmt.Errorf("run %s has invalid imported validation evidence: %w", run.ID, evidenceErr)
				}
				continue
			}
			if bindingErr := webevidence.ValidateReviewerBindings(nil, manifest.Reviewers, manifest.SecurityReviews, manifest.CrossExaminations); bindingErr != nil {
				if runID != "" {
					return record.Run{}, model.Decision{}, model.Manifest{}, fmt.Errorf("run %s has invalid reviewer bindings: %w", run.ID, bindingErr)
				}
				continue
			}
			if patchErr := record.ValidateCanonicalPatch(run, manifest.Target); patchErr != nil {
				if runID != "" {
					return record.Run{}, model.Decision{}, model.Manifest{}, fmt.Errorf("run %s has an invalid canonical patch: %w", run.ID, patchErr)
				}
				continue
			}
		}
		return run, decision, manifest, nil
	}
	return record.Run{}, model.Decision{}, model.Manifest{}, errors.New("no approved CORA run matches the requested head")
}

func manifestChecksPassed(checks []model.CheckResult) bool {
	for _, check := range checks {
		if check.Status != "passed" {
			return false
		}
	}
	return true
}

func printDecision(decision model.Decision) {
	stateLabel := strings.ToUpper(decision.State)
	if decision.State == stateDeltaApproved {
		stateLabel = "DELTA APPROVED — FINAL FULL REVIEW REQUIRED"
	} else if decision.State == model.StateApproved && decision.OutcomeQualifier == "non_blocking_findings" {
		stateLabel = "APPROVED WITH NON-BLOCKING FINDINGS"
	} else if decision.State == model.StateApproved && decision.OutcomeQualifier == "cross_examined" {
		stateLabel = "APPROVED AFTER CROSS-EXAMINATION"
	}
	fmt.Printf("%s %s\n", stateLabel, shortSHA(decision.HeadSHA))
	fmt.Printf("Reason: %s\n", decision.Reason)
	policy := "standard"
	if decision.StrictPolicy {
		policy = "strict"
	}
	validation := decision.ValidationStatus
	if validation == "" {
		validation = "not_recorded"
	}
	fmt.Printf("Policy: %s\nValidation: %s\n", policy, validation)
	reviewerErrors := loadReviewerErrors(decision)
	names := make([]string, 0, len(decision.Reviewers))
	for name := range decision.Reviewers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("%-8s %s\n", name+":", decision.Reviewers[name])
		if message := reviewerErrors[name]; message != "" {
			fmt.Printf("         Error: %s\n", message)
		}
	}
	checkNames := make([]string, 0, len(decision.Checks))
	for name := range decision.Checks {
		checkNames = append(checkNames, name)
	}
	sort.Strings(checkNames)
	for _, name := range checkNames {
		fmt.Printf("%-8s %s\n", "check:"+name, decision.Checks[name])
	}
	fmt.Printf("Findings: %s\n", formatDecisionFindingSummary(decision))
	if !usageEmpty(decision.IncrementalUsage) || !usageEmpty(decision.CumulativeUsage) {
		fmt.Printf("Usage this run: %s\n", formatUsage(decision.IncrementalUsage))
		fmt.Printf("Usage cumulative: %s\n", formatUsage(decision.CumulativeUsage))
	} else {
		fmt.Printf("Usage: %s\n", formatUsage(decision.Usage))
	}
	for _, disagreement := range decision.Disagreements {
		fmt.Printf("Disagreement: %s\n", disagreement)
	}
	for _, examination := range decision.CrossExaminations {
		fmt.Printf("Cross-examination: %s %s", examination.FindingID, examination.Status)
		if examination.Disposition != "" {
			fmt.Printf(" (%s: %s -> %s)", examination.Disposition, examination.OriginalSeverity, examination.EffectiveSeverity)
		}
		if examination.Error != "" {
			fmt.Printf(" — %s", examination.Error)
		}
		fmt.Println()
	}
	if decision.RecordPath != "" {
		fmt.Println("Record:", decision.RecordPath)
	}
}

func formatDecisionFindingSummary(decision model.Decision) string {
	blocking, nonBlocking := decisionFindingCounts(decision)
	return fmt.Sprintf("blocking=%d, non-blocking=%d (blocker=%d major=%d minor=%d note=%d)",
		blocking, nonBlocking, decision.OpenFindings["blocker"], decision.OpenFindings["major"],
		decision.OpenFindings["minor"], decision.OpenFindings["note"])
}

func decisionFindingCounts(decision model.Decision) (blocking, nonBlocking int) {
	total := 0
	for _, count := range decision.OpenFindings {
		total += count
	}
	if decision.BlockingFindings+decision.NonBlockingFindings > 0 || total == 0 {
		return decision.BlockingFindings, decision.NonBlockingFindings
	}

	// Decisions written before the aggregate count fields were introduced did
	// not retain arbitrary custom blocking severities. Preserve their standard
	// and strict-policy behavior when rendering historical records.
	blocking = decision.OpenFindings["blocker"] + decision.OpenFindings["major"]
	if decision.StrictPolicy {
		blocking += decision.OpenFindings["minor"]
	} else {
		nonBlocking += decision.OpenFindings["minor"]
	}
	nonBlocking += decision.OpenFindings["note"]
	return blocking, nonBlocking
}

func decisionForDisplay(decision model.Decision, reviewScope string) model.Decision {
	if reviewScope != "approved-baseline-delta" || decision.State != model.StateApproved {
		return decision
	}
	decision.State = stateDeltaApproved
	decision.OutcomeQualifier = "final_full_review_required"
	if strings.TrimSpace(decision.Reason) != "" {
		decision.Reason += "; "
	}
	decision.Reason += "auto-fix delta consensus is non-final until the complete updated diff passes a fresh full review"
	return decision
}

func usageEmpty(usage model.Usage) bool {
	return usage.Turns == 0 && usage.InputTokens == 0 && usage.CachedInputTokens == 0 && usage.OutputTokens == 0 &&
		usage.ThinkingTokens == 0 && usage.APIEquivalentCostUSD == 0 && !usage.TurnsKnown && !usage.TurnsPartial &&
		!usage.ThinkingTokensKnown && !usage.ThinkingTokensPartial && !usage.APIEquivalentCostKnown && !usage.APIEquivalentCostPartial
}

func formatUsage(usage model.Usage) string {
	return fmt.Sprintf("provider-turns=%s thinking=%s api-equivalent=%s", formatTurns(usage), formatThinking(usage), formatCost(usage))
}

func formatTurns(usage model.Usage) string {
	if usage.TurnsKnown {
		return fmt.Sprintf("%d", usage.Turns)
	}
	if usage.TurnsPartial {
		return fmt.Sprintf("%d (partial)", usage.Turns)
	}
	return "n/a"
}

func formatThinking(usage model.Usage) string {
	if usage.ThinkingTokensKnown {
		return fmt.Sprintf("%d", usage.ThinkingTokens)
	}
	if usage.ThinkingTokensPartial {
		return fmt.Sprintf("%d (partial)", usage.ThinkingTokens)
	}
	return "n/a"
}

func formatCost(usage model.Usage) string {
	if usage.APIEquivalentCostKnown {
		return fmt.Sprintf("$%.4f", usage.APIEquivalentCostUSD)
	}
	if usage.APIEquivalentCostPartial {
		return fmt.Sprintf("$%.4f (partial)", usage.APIEquivalentCostUSD)
	}
	return "n/a"
}

func loadReviewerErrors(decision model.Decision) map[string]string {
	if len(decision.ReviewerErrors) > 0 {
		return decision.ReviewerErrors
	}
	errorsByReviewer := make(map[string]string)
	if decision.RecordPath == "" {
		return errorsByReviewer
	}
	manifest, err := record.LoadManifest(record.Run{ID: decision.RunID, Path: decision.RecordPath})
	if err != nil {
		return errorsByReviewer
	}
	for _, reviewer := range manifestReviewerResults(manifest) {
		if reviewer.Error != "" {
			errorsByReviewer[reviewer.Reviewer] = reviewer.Error
		}
	}
	return errorsByReviewer
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func exitCodeForState(state string) int {
	switch state {
	case model.StateApproved:
		return 0
	case model.StateChangesRequested:
		return 2
	case model.StateNeedsHuman:
		return 3
	case model.StateIncomplete:
		return 4
	case model.StateStale:
		return 5
	case model.StatePaused:
		return 6
	default:
		return 10
	}
}
