package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	d.Duration = parsed
	return nil
}

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.String()), nil
}

type Reviewer struct {
	Enabled           bool    `toml:"enabled"`
	Command           string  `toml:"command"`
	Model             string  `toml:"model"`
	Effort            string  `toml:"effort"`
	MaxTurns          int     `toml:"max_turns"`
	FinalizationTurns int     `toml:"finalization_turns"`
	MaxBudgetUSD      float64 `toml:"max_budget_usd"`
	MaxConcurrency    int     `toml:"max_concurrency"`
}

type Reviewers struct {
	Codex  Reviewer `toml:"codex"`
	Claude Reviewer `toml:"claude"`
}

type Check struct {
	Name         string   `toml:"name"`
	Command      []string `toml:"command"`
	Timeout      Duration `toml:"timeout"`
	EnvAllowlist []string `toml:"env_allowlist"`
	Profile      string   `toml:"-"`
}

type ValidationProfile struct {
	Name   string  `toml:"name"`
	Checks []Check `toml:"checks"`
}

type Escalation struct {
	Enabled                 bool     `toml:"enabled"`
	Model                   string   `toml:"model"`
	Effort                  string   `toml:"effort"`
	MaxTurns                *int     `toml:"max_turns"`
	MaxBudgetUSD            *float64 `toml:"max_budget_usd"`
	SecurityPathMarkers     []string `toml:"security_path_markers"`
	ForceSecuritySensitive  bool     `toml:"-"`
	AdjudicateDisagreements bool     `toml:"adjudicate_disagreements"`
}

type CrossExamination struct {
	Timeout      Duration `toml:"timeout"`
	MaxTurns     int      `toml:"max_turns"`
	MaxBudgetUSD float64  `toml:"max_budget_usd"`
}

type AutoFix struct {
	Command        string   `toml:"command"`
	Model          string   `toml:"model"`
	Effort         string   `toml:"effort"`
	Threshold      string   `toml:"until"`
	AgentTimeout   Duration `toml:"agent_timeout"`
	MaxDuration    Duration `toml:"max_duration"`
	MaxIterations  int      `toml:"max_iterations"`
	MaxTurns       int      `toml:"max_turns"`
	MaxCostUSD     float64  `toml:"max_cost_usd"`
	MaxConcurrency int      `toml:"max_concurrency"`
}

type Config struct {
	Base                         string              `toml:"base"`
	ReviewerTimeout              Duration            `toml:"reviewer_timeout"`
	OverallTimeout               Duration            `toml:"overall_timeout"`
	QueueTimeout                 Duration            `toml:"queue_timeout"`
	StrictPolicy                 bool                `toml:"strict"`
	CrossExamineBlockingFindings bool                `toml:"cross_examine_blocking_findings"`
	RequireCleanTree             bool                `toml:"require_clean_tree"`
	AllowAPIBilling              bool                `toml:"allow_api_billing"`
	AllowUnsafeChecks            bool                `toml:"allow_unsafe_host_checks"`
	MinimumApprovals             int                 `toml:"minimum_approvals"`
	BlockingSeverities           []string            `toml:"blocking_severities"`
	PromptFile                   string              `toml:"prompt_file"`
	Reviewers                    Reviewers           `toml:"reviewers"`
	Escalation                   Escalation          `toml:"escalation"`
	CrossExamination             CrossExamination    `toml:"cross_examination"`
	AutoFix                      AutoFix             `toml:"auto_fix"`
	Checks                       []Check             `toml:"checks"`
	ValidationProfiles           []ValidationProfile `toml:"validation_profiles"`
	LoadedFiles                  []string            `toml:"-"`
}

func Defaults() Config {
	return Config{
		ReviewerTimeout:              Duration{Duration: 15 * time.Minute},
		OverallTimeout:               Duration{Duration: 45 * time.Minute},
		QueueTimeout:                 Duration{Duration: 24 * time.Hour},
		CrossExamineBlockingFindings: true,
		RequireCleanTree:             true,
		MinimumApprovals:             2,
		BlockingSeverities: []string{
			"blocker",
			"major",
		},
		Reviewers: Reviewers{
			Codex: Reviewer{
				Enabled:        true,
				Command:        "codex",
				Model:          "gpt-5.6-sol",
				Effort:         "high",
				MaxConcurrency: 2,
			},
			Claude: Reviewer{
				Enabled:           true,
				Command:           "claude",
				Model:             "opus",
				Effort:            "high",
				MaxTurns:          50,
				FinalizationTurns: 2,
				MaxConcurrency:    1,
			},
		},
		Escalation: Escalation{
			Enabled:                 true,
			Model:                   "fable",
			Effort:                  "high",
			AdjudicateDisagreements: false,
			SecurityPathMarkers: []string{
				"/.cora/", "/.codex/", "/.claude/", "/.github/workflows/",
				"/auth/", "/authentication/", "/authorization/", "/security/",
				"/crypto/", "/cryptography/", "/iam/", "/permissions/",
				"/secrets/", "/credentials/", "oauth", "jwt",
			},
		},
		CrossExamination: CrossExamination{
			Timeout: Duration{Duration: 10 * time.Minute}, MaxTurns: 20, MaxBudgetUSD: 5,
		},
		AutoFix: AutoFix{
			Command: "codex", Model: "gpt-5.6-sol", Effort: "high", Threshold: "major",
			AgentTimeout: Duration{Duration: 20 * time.Minute}, MaxDuration: Duration{Duration: time.Hour},
			MaxIterations: 5, MaxTurns: 250, MaxCostUSD: 50, MaxConcurrency: 1,
		},
	}
}

func Load(repoRoot string) (Config, error) {
	cfg, err := LoadPersonal()
	if err != nil {
		return Config{}, err
	}

	if repoRoot != "" {
		if err := decodeIfPresent(filepath.Join(repoRoot, ".cora", "config.toml"), &cfg); err != nil {
			return Config{}, err
		}
	}
	return finalize(cfg)
}

// LoadPersonal loads defaults and the platform-specific personal config. It
// intentionally does not validate the intermediate result because a trusted
// repository config may complete or override it.
func LoadPersonal() (Config, error) {
	cfg := Defaults()
	userPath, err := UserPath()
	if err == nil {
		if err := decodeIfPresent(userPath, &cfg); err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
}

// ApplyRepository applies repository config bytes obtained from a trusted Git
// revision, then fills derived defaults and validates the effective config.
func ApplyRepository(cfg Config, source string, contents []byte) (Config, error) {
	if source != "" {
		if err := decode(contents, source, &cfg); err != nil {
			return Config{}, err
		}
	}
	return finalize(cfg)
}

// UserPath returns the platform-specific path to the personal CORA config.
func UserPath() (string, error) {
	userDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(userDir, "cora", "config.toml"), nil
}

func decodeIfPresent(path string, cfg *Config) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("config path is a directory: %s", path)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}
	return decode(contents, path, cfg)
}

func decode(contents []byte, source string, cfg *Config) error {
	metadata, err := toml.Decode(string(contents), cfg)
	if err != nil {
		return fmt.Errorf("decode config %s: %w", source, err)
	}
	if undecoded := metadata.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, key := range undecoded {
			keys = append(keys, key.String())
		}
		return fmt.Errorf("decode config %s: unknown keys: %s", source, strings.Join(keys, ", "))
	}
	cfg.LoadedFiles = append(cfg.LoadedFiles, source)
	return nil
}

func finalize(cfg Config) (Config, error) {
	for i := range cfg.Checks {
		if cfg.Checks[i].Timeout.Duration == 0 {
			cfg.Checks[i].Timeout.Duration = 10 * time.Minute
		}
	}
	for profileIndex := range cfg.ValidationProfiles {
		for checkIndex := range cfg.ValidationProfiles[profileIndex].Checks {
			check := &cfg.ValidationProfiles[profileIndex].Checks[checkIndex]
			if check.Timeout.Duration == 0 {
				check.Timeout.Duration = 10 * time.Minute
			}
		}
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.ReviewerTimeout.Duration <= 0 {
		return errors.New("reviewer_timeout must be positive")
	}
	if c.OverallTimeout.Duration <= 0 {
		return errors.New("overall_timeout must be positive")
	}
	if c.QueueTimeout.Duration <= 0 {
		return errors.New("queue_timeout must be positive")
	}
	enabledReviewers := 0
	if c.Reviewers.Codex.Enabled {
		enabledReviewers++
	}
	if c.Reviewers.Claude.Enabled {
		enabledReviewers++
	}
	if enabledReviewers == 0 {
		return errors.New("at least one reviewer must be enabled")
	}
	if c.MinimumApprovals < 1 || c.MinimumApprovals > enabledReviewers {
		return fmt.Errorf("minimum_approvals must be between 1 and the %d enabled reviewers", enabledReviewers)
	}
	if c.Reviewers.Codex.Enabled && strings.TrimSpace(c.Reviewers.Codex.Command) == "" {
		return errors.New("reviewers.codex.command cannot be empty")
	}
	if c.Reviewers.Claude.Enabled && strings.TrimSpace(c.Reviewers.Claude.Command) == "" {
		return errors.New("reviewers.claude.command cannot be empty")
	}
	if c.Reviewers.Claude.MaxTurns < 1 {
		return errors.New("reviewers.claude.max_turns must be positive")
	}
	if c.Reviewers.Claude.FinalizationTurns < 1 || c.Reviewers.Claude.FinalizationTurns >= c.Reviewers.Claude.MaxTurns {
		return errors.New("reviewers.claude.finalization_turns must be positive and less than max_turns")
	}
	if c.Reviewers.Claude.MaxBudgetUSD < 0 {
		return errors.New("reviewers.claude.max_budget_usd cannot be negative")
	}
	if c.Reviewers.Codex.Enabled && c.Reviewers.Codex.MaxConcurrency < 1 {
		return errors.New("reviewers.codex.max_concurrency must be positive")
	}
	if c.Reviewers.Claude.Enabled && c.Reviewers.Claude.MaxConcurrency < 1 {
		return errors.New("reviewers.claude.max_concurrency must be positive")
	}
	if err := validateEffort("reviewers.codex.effort", c.Reviewers.Codex.Effort, true); err != nil {
		return err
	}
	if err := validateEffort("reviewers.claude.effort", c.Reviewers.Claude.Effort, false); err != nil {
		return err
	}
	if c.Escalation.Enabled || c.CrossExamineBlockingFindings {
		if strings.TrimSpace(c.Escalation.Model) == "" {
			return errors.New("escalation.model cannot be empty when escalation or cross-examination is enabled")
		}
		if err := validateEffort("escalation.effort", c.Escalation.Effort, false); err != nil {
			return err
		}
		if c.Escalation.MaxTurns != nil {
			if *c.Escalation.MaxTurns < 1 {
				return errors.New("escalation.max_turns must be positive when set")
			}
			if *c.Escalation.MaxTurns <= c.Reviewers.Claude.FinalizationTurns {
				return errors.New("escalation.max_turns must be greater than reviewers.claude.finalization_turns")
			}
		}
		if c.Escalation.MaxBudgetUSD != nil && *c.Escalation.MaxBudgetUSD < 0 {
			return errors.New("escalation.max_budget_usd cannot be negative")
		}
	}
	if c.CrossExamination.Timeout.Duration <= 0 {
		return errors.New("cross_examination.timeout must be positive")
	}
	if c.CrossExamination.MaxTurns <= c.Reviewers.Claude.FinalizationTurns {
		return errors.New("cross_examination.max_turns must be greater than reviewers.claude.finalization_turns")
	}
	if c.CrossExamination.MaxBudgetUSD < 0 {
		return errors.New("cross_examination.max_budget_usd cannot be negative")
	}
	if strings.TrimSpace(c.AutoFix.Command) == "" {
		return errors.New("auto_fix.command cannot be empty")
	}
	if strings.TrimSpace(c.AutoFix.Model) == "" {
		return errors.New("auto_fix.model cannot be empty")
	}
	if err := validateEffort("auto_fix.effort", c.AutoFix.Effort, true); err != nil {
		return err
	}
	switch c.AutoFix.Threshold {
	case "blocker", "major", "minor":
	default:
		return errors.New("auto_fix.until must be blocker, major, or minor")
	}
	if c.AutoFix.AgentTimeout.Duration <= 0 {
		return errors.New("auto_fix.agent_timeout must be positive")
	}
	if c.AutoFix.MaxDuration.Duration <= 0 {
		return errors.New("auto_fix.max_duration must be positive")
	}
	if c.AutoFix.MaxIterations < 1 {
		return errors.New("auto_fix.max_iterations must be positive")
	}
	if c.AutoFix.MaxTurns < 1 {
		return errors.New("auto_fix.max_turns must be positive")
	}
	if c.AutoFix.MaxCostUSD <= 0 {
		return errors.New("auto_fix.max_cost_usd must be positive")
	}
	if c.AutoFix.MaxConcurrency < 1 {
		return errors.New("auto_fix.max_concurrency must be positive")
	}
	if len(c.BlockingSeverities) == 0 {
		return errors.New("blocking_severities cannot be empty")
	}
	checkNames := make(map[string]bool, len(c.Checks))
	for i, check := range c.Checks {
		if err := validateCheck(check, fmt.Sprintf("checks[%d]", i)); err != nil {
			return err
		}
		if checkNames[check.Name] {
			return fmt.Errorf("checks[%d].name duplicates %q", i, check.Name)
		}
		checkNames[check.Name] = true
	}
	profileNames := make(map[string]bool, len(c.ValidationProfiles))
	for profileIndex, profile := range c.ValidationProfiles {
		if strings.TrimSpace(profile.Name) == "" {
			return fmt.Errorf("validation_profiles[%d].name cannot be empty", profileIndex)
		}
		if profileNames[profile.Name] {
			return fmt.Errorf("validation_profiles[%d].name duplicates %q", profileIndex, profile.Name)
		}
		if len(profile.Checks) == 0 {
			return fmt.Errorf("validation_profiles[%d].checks cannot be empty", profileIndex)
		}
		for checkIndex, check := range profile.Checks {
			if err := validateCheck(check, fmt.Sprintf("validation_profiles[%d].checks[%d]", profileIndex, checkIndex)); err != nil {
				return err
			}
		}
		profileNames[profile.Name] = true
	}
	return nil
}

func validateCheck(check Check, location string) error {
	if strings.TrimSpace(check.Name) == "" {
		return fmt.Errorf("%s.name cannot be empty", location)
	}
	if len(check.Command) == 0 || strings.TrimSpace(check.Command[0]) == "" {
		return fmt.Errorf("%s.command cannot be empty", location)
	}
	if check.Timeout.Duration <= 0 {
		return fmt.Errorf("%s.timeout must be positive", location)
	}
	environmentNames := make(map[string]bool, len(check.EnvAllowlist))
	for _, name := range check.EnvAllowlist {
		if !validEnvironmentName(name) {
			return fmt.Errorf("%s.env_allowlist contains invalid environment variable %q", location, name)
		}
		if environmentNames[name] {
			return fmt.Errorf("%s.env_allowlist duplicates %q", location, name)
		}
		environmentNames[name] = true
	}
	return nil
}

// ApplyProfiles appends trusted named validation profiles. Built-in profiles
// remain available when a repository has no .cora/config.toml.
func ApplyProfiles(cfg Config, names []string) (Config, error) {
	profiles := map[string]ValidationProfile{
		"go": {
			Name: "go",
			Checks: []Check{
				{Name: "go-test", Command: []string{"go", "test", "./..."}, Timeout: Duration{Duration: 15 * time.Minute}},
				{Name: "go-vet", Command: []string{"go", "vet", "./..."}, Timeout: Duration{Duration: 10 * time.Minute}},
			},
		},
		"node": {
			Name: "node",
			Checks: []Check{
				{Name: "node-test", Command: []string{"npm", "test"}, Timeout: Duration{Duration: 20 * time.Minute}},
			},
		},
		"python": {
			Name: "python",
			Checks: []Check{
				{Name: "python-test", Command: []string{"python3", "-m", "pytest"}, Timeout: Duration{Duration: 20 * time.Minute}},
			},
		},
	}
	for _, profile := range cfg.ValidationProfiles {
		profiles[profile.Name] = profile
	}
	seenChecks := make(map[string]bool, len(cfg.Checks))
	for _, check := range cfg.Checks {
		seenChecks[check.Name] = true
	}
	for _, name := range names {
		profile, found := profiles[name]
		if !found {
			return Config{}, fmt.Errorf("unknown validation profile %q", name)
		}
		for _, check := range profile.Checks {
			if seenChecks[check.Name] {
				continue
			}
			check.Profile = profile.Name
			cfg.Checks = append(cfg.Checks, check)
			seenChecks[check.Name] = true
		}
	}
	return finalize(cfg)
}

func validateEffort(name, effort string, allowMinimal bool) error {
	if effort == "" {
		return nil
	}
	allowed := map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}
	if allowMinimal {
		allowed["minimal"] = true
		allowed["none"] = true
	}
	if !allowed[effort] {
		return fmt.Errorf("%s must be one of %s", name, effortChoices(allowMinimal))
	}
	return nil
}

func effortChoices(includeMinimal bool) string {
	if includeMinimal {
		return "none, minimal, low, medium, high, xhigh, or max"
	}
	return "low, medium, high, xhigh, or max"
}

func validEnvironmentName(name string) bool {
	if name == "" {
		return false
	}
	for index, character := range name {
		if character == '_' || character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}
