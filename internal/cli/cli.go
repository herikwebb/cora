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
	"github.com/herikwebb/cora/internal/record"
	"github.com/spf13/cobra"
)

var Version = "0.1.0-dev"

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
	root.PersistentFlags().StringVarP(&opts.repo, "repo", "C", ".", "target repository directory")
	root.PersistentFlags().BoolVar(&opts.json, "json", false, "emit machine-readable JSON")
	root.AddCommand(newReviewCommand(opts))
	root.AddCommand(newStatusCommand(opts))
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
			target, err = resolve(candidateBase, cfg.RequireCleanTree)
			if err != nil {
				return err
			}
			decision, err := (orchestrator.Runner{Version: Version, Progress: command.ErrOrStderr()}).Run(ctx, repo, target, cfg)
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
	return command
}

func newStatusCommand(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the latest review run",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			repo, err := gitx.Discover(command.Context(), opts.repo)
			if err != nil {
				return err
			}
			store := record.New(repo.CommonDir)
			run, err := store.Resolve("latest")
			if err != nil {
				return err
			}
			decision, err := record.LoadDecision(run)
			if err != nil {
				return err
			}
			if opts.json {
				return printJSON(decision)
			}
			printDecision(decision)
			return nil
		},
	}
}

func newShowCommand(opts *options) *cobra.Command {
	return &cobra.Command{
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
					for _, finding := range reviewer.Report.Findings {
						fmt.Printf("  [%s] %s:%d %s\n", finding.Severity, finding.File, finding.Line, finding.Claim)
					}
				} else {
					fmt.Printf("\n%s: incomplete (model=%s effort=%s) — %s\n", strings.ToUpper(reviewer.Reviewer), modelName, effort, reviewer.Error)
				}
				if reviewer.EscalationCause != "" {
					fmt.Printf("Escalation: %s\n", reviewer.EscalationCause)
				}
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
	fmt.Printf("%s %s\n", strings.ToUpper(decision.State), shortSHA(decision.HeadSHA))
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
	fmt.Printf("Usage: %s\n", formatUsage(decision.Usage))
	if decision.RecordPath != "" {
		fmt.Println("Record:", decision.RecordPath)
	}
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
