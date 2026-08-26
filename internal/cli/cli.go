package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/herikwebb/cora/internal/config"
	"github.com/herikwebb/cora/internal/gitx"
	"github.com/herikwebb/cora/internal/model"
	"github.com/herikwebb/cora/internal/orchestrator"
	"github.com/herikwebb/cora/internal/provider"
	"github.com/herikwebb/cora/internal/record"
	"github.com/spf13/cobra"
)

var Version = "0.1.0-dev"
var SourceSHA = "unknown"
var BuildTime = "unknown"

type options struct {
	repo string
	json bool
}

type stateError struct {
	state string
}

func (e stateError) Error() string { return e.state }

func Execute() int {
	root := newRootCommand()
	root.SilenceErrors = true
	root.SilenceUsage = true
	if err := root.Execute(); err != nil {
		var state stateError
		if errors.As(err, &state) {
			return exitCodeForState(state.state)
		}
		fmt.Fprintln(os.Stderr, "cora:", err)
		return 10
	}
	return 0
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
	var allowUnsafeChecks bool
	var securitySensitive bool
	var adjudicate bool
	var profiles []string
	command := &cobra.Command{
		Use:   "review",
		Short: "Review changes independently and evaluate consensus",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			ctx := command.Context()
			repo, err := gitx.Discover(ctx, opts.repo)
			if err != nil {
				return err
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
			if allowAPIBilling {
				cfg.AllowAPIBilling = true
			}
			if allowUnsafeChecks {
				cfg.AllowUnsafeChecks = true
			}
			if securitySensitive {
				cfg.Escalation.ForceSecuritySensitive = true
			}
			if adjudicate {
				cfg.Escalation.AdjudicateDisagreements = true
			}
			target, err = resolve(candidateBase, cfg.RequireCleanTree)
			if err != nil {
				return err
			}
			profiles, err = expandAutoProfiles(ctx, repo, target, profiles)
			if err != nil {
				return err
			}
			cfg, err = config.ApplyProfiles(cfg, profiles)
			if err != nil {
				return err
			}
			decision, err := runner(command).Run(ctx, repo, target, cfg)
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
	command.Flags().BoolVar(&allowUnsafeChecks, "allow-unsafe-checks", false, "allow configured checks to execute unsandboxed on the host")
	command.Flags().BoolVar(&securitySensitive, "security-sensitive", false, "escalate Claude to the configured security review model and effort")
	command.Flags().BoolVar(&adjudicate, "adjudicate", false, "run a Fable adjudicator when reviewers disagree")
	command.Flags().StringSliceVar(&profiles, "profile", nil, "validation profile to run (repeatable; auto detects a built-in profile)")
	return command
}

func newRetryCommand(opts *options) *cobra.Command {
	var reviewers []string
	var noWait bool
	var allowAPIBilling bool
	var adjudicate bool
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
			if parentManifest.FinishedAt.IsZero() {
				return fmt.Errorf("run %s is still active or did not finish", parentRun.ID)
			}
			valid, err := repo.VerifyTarget(ctx, parentManifest.Target)
			if err != nil {
				return err
			}
			if !valid {
				return fmt.Errorf("run %s no longer matches its recorded Git target", parentRun.ID)
			}
			selected, err := selectRetryReviewers(parentManifest.Reviewers, reviewers)
			if err != nil {
				return err
			}
			notBefore := quotaNotBefore(parentManifest.Reviewers, selected, parentManifest.FinishedAt, time.Now())
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
			cfg, err := loadTrustedConfig(ctx, repo, personal, parentManifest.Target)
			if err != nil {
				return err
			}
			if allowAPIBilling {
				cfg.AllowAPIBilling = true
			}
			if adjudicate {
				cfg.Escalation.AdjudicateDisagreements = true
			}
			if (selected["codex"] && !cfg.Reviewers.Codex.Enabled) || (selected["claude"] && !cfg.Reviewers.Claude.Enabled) {
				return errors.New("selected reviewer is disabled by the trusted configuration")
			}
			previous := make([]model.ReviewerResult, len(parentManifest.Reviewers))
			copy(previous, parentManifest.Reviewers)
			for index := range previous {
				if selected[previous[index].Reviewer] {
					previous[index].Status = "incomplete"
					previous[index].Report = nil
					previous[index].ReusedFromRunID = ""
				}
			}
			decision, err := runner(command).RunWithOptions(ctx, repo, parentManifest.Target, cfg, orchestrator.RunOptions{
				ParentRunID: parentRun.ID, RetryReviewers: selected, ReuseReviewers: previous,
				ReuseChecks: true, Checks: parentManifest.Checks, NotBefore: notBefore,
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
	command.Flags().StringSliceVar(&reviewers, "reviewer", nil, "reviewer to retry: codex or claude (repeatable)")
	command.Flags().BoolVar(&noWait, "no-wait", false, "return instead of waiting for a recorded provider quota reset")
	command.Flags().BoolVar(&allowAPIBilling, "allow-api-billing", false, "allow API-key or other separately billed authentication")
	command.Flags().BoolVar(&adjudicate, "adjudicate", false, "run a Fable adjudicator when reviewers disagree")
	return command
}

func runner(command *cobra.Command) orchestrator.Runner {
	return orchestrator.Runner{Version: Version, SourceSHA: SourceSHA, BuildTime: BuildTime, Progress: command.ErrOrStderr()}
}

func selectRetryReviewers(results []model.ReviewerResult, requested []string) (map[string]bool, error) {
	available := make(map[string]model.ReviewerResult)
	for _, result := range results {
		if result.Reviewer == "codex" || result.Reviewer == "claude" {
			available[result.Reviewer] = result
		}
	}
	selected := make(map[string]bool)
	if len(requested) == 0 {
		for name, result := range available {
			if result.Status != "completed" || result.Report == nil {
				selected[name] = true
			}
		}
		if len(selected) == 0 {
			return nil, errors.New("all base reviewers completed; pass --reviewer to rerun one explicitly")
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
		if _, found, err := repo.ReadFileAt(ctx, target.HeadSHA, "go.mod"); err != nil {
			return nil, err
		} else if found && !seen["go"] {
			expanded = append(expanded, "go")
			seen["go"] = true
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
				activeRuns := make([]model.RunSummary, 0)
				for _, summary := range summaries {
					if summary.State == "active" {
						activeRuns = append(activeRuns, summary)
					}
				}
				if opts.json {
					return printJSON(activeRuns)
				}
				if len(activeRuns) == 0 {
					fmt.Fprintln(command.OutOrStdout(), "No active CORA runs.")
					return nil
				}
				for _, summary := range activeRuns {
					printRunSummary(summary)
				}
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
			if opts.json {
				return printJSON(decision)
			}
			printDecision(decision)
			return nil
		},
	}
	command.Flags().BoolVar(&active, "active", false, "show all currently active runs")
	return command
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
			fmt.Fprintln(command.OutOrStdout(), "RUN                                      STATE               HEAD      ELAPSED   PARENT")
			for _, summary := range filtered {
				parent := summary.ParentRunID
				if parent == "" {
					parent = "-"
				}
				fmt.Fprintf(command.OutOrStdout(), "%-40s %-19s %-9s %-9s %s\n", summary.RunID, summary.State, shortSHA(summary.HeadSHA), formatMilliseconds(summary.ElapsedMS), parent)
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

func loadRunSummary(run record.Run) (model.RunSummary, error) {
	manifest, err := record.LoadManifest(run)
	if err != nil {
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
		ParentRunID: manifest.ParentRunID, RepositoryIdentity: manifest.RepositoryIdentity, RecordPath: run.Path,
	}
	if decision, decisionErr := record.LoadDecision(run); decisionErr == nil {
		summary.State = decision.State
		summary.Reviewers = decision.Reviewers
		summary.Checks = decision.Checks
		return summary, nil
	}
	if heartbeat, heartbeatErr := record.LoadHeartbeat(run); heartbeatErr == nil {
		summary.State = heartbeat.State
		summary.Phase = heartbeat.Phase
		summary.Reviewers = heartbeat.Reviewers
		summary.Checks = heartbeat.Checks
		summary.Queues = heartbeat.Queues
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
	fmt.Printf("Head: %s\nElapsed: %s\nRecord: %s\n", shortSHA(summary.HeadSHA), formatMilliseconds(summary.ElapsedMS), summary.RecordPath)
	for _, name := range sortedStateNames(summary.Reviewers) {
		fmt.Printf("%-18s %s\n", name+":", summary.Reviewers[name])
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
		eta := "unknown"
		if queue.ETAAt != nil {
			eta = queue.ETAAt.Local().Format(time.RFC3339)
		}
		fmt.Printf("%-18s position=%d ahead=%d active=%d/%d eta=%s\n", "queue "+name+":", queue.Position, queue.Ahead, queue.Active, queue.Limit, eta)
	}
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
			view := struct {
				Manifest model.Manifest `json:"manifest"`
				Decision model.Decision `json:"decision"`
			}{Manifest: manifest, Decision: decision}
			if opts.json {
				return printJSON(view)
			}
			printDecision(decision)
			fmt.Printf("Started:    %s\n", manifest.StartedAt.Local().Format(time.RFC3339))
			fmt.Printf("Finished:   %s\n", manifest.FinishedAt.Local().Format(time.RFC3339))
			if len(decision.Findings) > 0 {
				fmt.Println("\nConsolidated findings:")
				for _, finding := range decision.Findings {
					fmt.Printf("  [%s] %s:%d %s (%s)\n", finding.Severity, finding.File, finding.Line, finding.Claim, strings.Join(finding.Reviewers, ", "))
					if verbose {
						fmt.Printf("    Confidence: %.0f%%\n", finding.Confidence*100)
						for _, evidence := range finding.Evidence {
							fmt.Printf("    Evidence: %s\n", evidence)
						}
						for _, fix := range finding.SuggestedFixes {
							fmt.Printf("    Suggested fix: %s\n", fix)
						}
					}
				}
			}
			if verbose && len(decision.ResidualRisks) > 0 {
				fmt.Println("\nResidual risks:")
				for _, risk := range decision.ResidualRisks {
					fmt.Printf("  - %s\n", risk)
				}
			}
			for _, reviewer := range manifest.Reviewers {
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
				fmt.Printf("Duration: execution=%s queue=%s total=%s\n", formatMilliseconds(reviewer.ExecutionDuration.Milliseconds()), formatMilliseconds(reviewer.QueueDuration.Milliseconds()), formatMilliseconds(reviewer.Duration.Milliseconds()))
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
				}
			}
			return nil
		},
	}
	command.Flags().BoolVarP(&verbose, "verbose", "v", false, "show evidence, suggested fixes, omitted paths, and residual risks")
	return command
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
			resolvedHead, err := repo.ResolveRevision(ctx, head)
			if err != nil {
				return err
			}
			store := record.New(repo.CommonDir)
			run, decision, manifest, err := findApproval(store, runID, resolvedHead)
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

func findApproval(store record.Store, runID, headSHA string) (record.Run, model.Decision, model.Manifest, error) {
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
		manifest, err := record.LoadManifest(run)
		if err != nil {
			continue
		}
		return run, decision, manifest, nil
	}
	return record.Run{}, model.Decision{}, model.Manifest{}, errors.New("no approved CORA run matches the requested head")
}

func printDecision(decision model.Decision) {
	stateLabel := strings.ToUpper(decision.State)
	if decision.State == model.StateApproved && decision.OutcomeQualifier == "non_blocking_findings" {
		stateLabel = "APPROVED WITH NON-BLOCKING FINDINGS"
	}
	fmt.Printf("%s %s\n", stateLabel, shortSHA(decision.HeadSHA))
	fmt.Printf("Reason: %s\n", decision.Reason)
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
	fmt.Printf("Findings: blocker=%d major=%d minor=%d note=%d\n",
		decision.OpenFindings["blocker"], decision.OpenFindings["major"], decision.OpenFindings["minor"], decision.OpenFindings["note"])
	if !usageEmpty(decision.IncrementalUsage) || !usageEmpty(decision.CumulativeUsage) {
		fmt.Printf("Usage this run: %s\n", formatUsage(decision.IncrementalUsage))
		fmt.Printf("Usage cumulative: %s\n", formatUsage(decision.CumulativeUsage))
	} else {
		fmt.Printf("Usage: %s\n", formatUsage(decision.Usage))
	}
	for _, disagreement := range decision.Disagreements {
		fmt.Printf("Disagreement: %s\n", disagreement)
	}
	if decision.RecordPath != "" {
		fmt.Println("Record:", decision.RecordPath)
	}
}

func usageEmpty(usage model.Usage) bool {
	return usage.Turns == 0 && usage.InputTokens == 0 && usage.CachedInputTokens == 0 && usage.OutputTokens == 0 &&
		usage.ThinkingTokens == 0 && usage.APIEquivalentCostUSD == 0 && !usage.TurnsKnown && !usage.TurnsPartial &&
		!usage.ThinkingTokensKnown && !usage.ThinkingTokensPartial && !usage.APIEquivalentCostKnown && !usage.APIEquivalentCostPartial
}

func formatUsage(usage model.Usage) string {
	return fmt.Sprintf("turns=%s thinking=%s api-equivalent=%s", formatTurns(usage), formatThinking(usage), formatCost(usage))
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
	for _, reviewer := range manifest.Reviewers {
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
	default:
		return 10
	}
}
