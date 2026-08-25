package cora

import _ "embed"

// ReviewSchema is the JSON schema every reviewer must satisfy.
//
//go:embed schemas/review-v1.json
var ReviewSchema []byte

// DefaultReviewPrompt is used when a repository does not provide an override.
//
//go:embed prompts/default-review.md
var DefaultReviewPrompt string
