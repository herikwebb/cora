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
	Enabled  bool   `toml:"enabled"`
	Command  string `toml:"command"`
	Model    string `toml:"model"`
	Effort   string `toml:"effort"`
	MaxTurns int    `toml:"max_turns"`
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
}

type Escalation struct {
	Enabled                bool     `toml:"enabled"`
	Model                  string   `toml:"model"`
	Effort                 string   `toml:"effort"`
	SecurityPathMarkers    []string `toml:"security_path_markers"`
	ForceSecuritySensitive bool     `toml:"-"`
}

type Config struct {
	Base               string     `toml:"base"`
	ReviewerTimeout    Duration   `toml:"reviewer_timeout"`
	OverallTimeout     Duration   `toml:"overall_timeout"`
	RequireCleanTree   bool       `toml:"require_clean_tree"`
	AllowAPIBilling    bool       `toml:"allow_api_billing"`
	AllowUnsafeChecks  bool       `toml:"allow_unsafe_host_checks"`
	MinimumApprovals   int        `toml:"minimum_approvals"`
	BlockingSeverities []string   `toml:"blocking_severities"`
	PromptFile         string     `toml:"prompt_file"`
	Reviewers          Reviewers  `toml:"reviewers"`
	Escalation         Escalation `toml:"escalation"`
	Checks             []Check    `toml:"checks"`
	LoadedFiles        []string   `toml:"-"`
}

func Defaults() Config {
	return Config{
		ReviewerTimeout:  Duration{Duration: 15 * time.Minute},
		OverallTimeout:   Duration{Duration: 45 * time.Minute},
		RequireCleanTree: true,
		MinimumApprovals: 2,
		BlockingSeverities: []string{
			"blocker",
			"major",
		},
		Reviewers: Reviewers{
			Codex: Reviewer{
				Enabled: true,
				Command: "codex",
				Model:   "gpt-5.6",
				Effort:  "high",
			},
			Claude: Reviewer{
				Enabled:  true,
				Command:  "claude",
				Model:    "opus",
				Effort:   "high",
				MaxTurns: 50,
			},
		},
		Escalation: Escalation{
			Enabled: true,
			Model:   "fable",
			Effort:  "high",
			SecurityPathMarkers: []string{
				"/.cora/", "/.codex/", "/.claude/", "/.github/workflows/",
				"/auth/", "/authentication/", "/authorization/", "/security/",
				"/crypto/", "/cryptography/", "/iam/", "/permissions/",
				"/secrets/", "/credentials/", "oauth", "jwt",
			},
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
	if err := validateEffort("reviewers.codex.effort", c.Reviewers.Codex.Effort, true); err != nil {
		return err
	}
	if err := validateEffort("reviewers.claude.effort", c.Reviewers.Claude.Effort, false); err != nil {
		return err
	}
	if c.Escalation.Enabled {
		if strings.TrimSpace(c.Escalation.Model) == "" {
			return errors.New("escalation.model cannot be empty when escalation is enabled")
		}
		if err := validateEffort("escalation.effort", c.Escalation.Effort, false); err != nil {
			return err
		}
	}
	if len(c.BlockingSeverities) == 0 {
		return errors.New("blocking_severities cannot be empty")
	}
	checkNames := make(map[string]bool, len(c.Checks))
	for i, check := range c.Checks {
		if strings.TrimSpace(check.Name) == "" {
			return fmt.Errorf("checks[%d].name cannot be empty", i)
		}
		if len(check.Command) == 0 || strings.TrimSpace(check.Command[0]) == "" {
			return fmt.Errorf("checks[%d].command cannot be empty", i)
		}
		if check.Timeout.Duration <= 0 {
			return fmt.Errorf("checks[%d].timeout must be positive", i)
		}
		if checkNames[check.Name] {
			return fmt.Errorf("checks[%d].name duplicates %q", i, check.Name)
		}
		environmentNames := make(map[string]bool, len(check.EnvAllowlist))
		for _, name := range check.EnvAllowlist {
			if !validEnvironmentName(name) {
				return fmt.Errorf("checks[%d].env_allowlist contains invalid environment variable %q", i, name)
			}
			if environmentNames[name] {
				return fmt.Errorf("checks[%d].env_allowlist duplicates %q", i, name)
			}
			environmentNames[name] = true
		}
		checkNames[check.Name] = true
	}
	return nil
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
