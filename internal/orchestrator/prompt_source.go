package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	coraassets "github.com/herikwebb/cora"
	"github.com/herikwebb/cora/internal/config"
	"github.com/herikwebb/cora/internal/gitx"
)

// ResolveReviewPromptSource validates the trusted prompt that a review would
// load and returns an audit-friendly source name without exposing its content.
// Planning uses this before declaring a review ready.
func ResolveReviewPromptSource(ctx context.Context, repo gitx.Repo, cfg config.Config, trustedBaseSHA string) (string, error) {
	_, source, err := resolveReviewPrompt(ctx, repo, cfg, trustedBaseSHA)
	return source, err
}

func resolveReviewPrompt(ctx context.Context, repo gitx.Repo, cfg config.Config, trustedBaseSHA string) (string, string, error) {
	if cfg.PromptFile != "" {
		if filepath.IsAbs(cfg.PromptFile) {
			contents, err := os.ReadFile(cfg.PromptFile)
			if err != nil {
				return "", "", fmt.Errorf("read review prompt %s: %w", cfg.PromptFile, err)
			}
			return string(contents), cfg.PromptFile, nil
		}
		contents, found, err := repo.ReadFileAt(ctx, trustedBaseSHA, cfg.PromptFile)
		if err != nil {
			return "", "", fmt.Errorf("read trusted review prompt %s: %w", cfg.PromptFile, err)
		}
		if !found {
			return "", "", fmt.Errorf("trusted review prompt %s does not exist at base %s", cfg.PromptFile, trustedBaseSHA)
		}
		return string(contents), fmt.Sprintf("git:%s:%s", trustedBaseSHA, cfg.PromptFile), nil
	}
	contents, found, err := repo.ReadFileAt(ctx, trustedBaseSHA, ".cora/reviewer.md")
	if err != nil {
		return "", "", fmt.Errorf("read trusted repository review prompt: %w", err)
	}
	if found {
		return string(contents), fmt.Sprintf("git:%s:.cora/reviewer.md", trustedBaseSHA), nil
	}
	return string(coraassets.DefaultReviewPrompt), "embedded:prompts/default-review.md", nil
}
