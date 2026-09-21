package cora

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/herikwebb/cora/internal/model"
)

func TestReviewSchemaUsesCodexCompatibleObjectConstraints(t *testing.T) {
	var schema any
	if err := json.Unmarshal(ReviewSchema, &schema); err != nil {
		t.Fatalf("parse review schema: %v", err)
	}

	unsupported := map[string]bool{"allOf": true, "if": true, "then": true}
	var inspect func(any, string) error
	inspect = func(value any, path string) error {
		switch value := value.(type) {
		case map[string]any:
			if properties, ok := value["properties"].(map[string]any); ok {
				requiredValues, ok := value["required"].([]any)
				if !ok {
					return fmt.Errorf("object with properties at %s has no required array", path)
				}
				required := make([]string, 0, len(requiredValues))
				for _, item := range requiredValues {
					name, ok := item.(string)
					if !ok {
						return fmt.Errorf("non-string required property at %s", path)
					}
					required = append(required, name)
				}
				for name := range properties {
					if !slices.Contains(required, name) {
						return fmt.Errorf("property %q at %s is not required", name, path)
					}
				}
			}
			for key, child := range value {
				if unsupported[key] {
					return fmt.Errorf("unsupported Codex schema keyword %q at %s", key, path)
				}
				if err := inspect(child, path+"."+key); err != nil {
					return err
				}
			}
		case []any:
			for index, child := range value {
				if err := inspect(child, fmt.Sprintf("%s[%d]", path, index)); err != nil {
					return err
				}
			}
		}
		return nil
	}

	if err := inspect(schema, "$"); err != nil {
		t.Fatal(err)
	}
}

func TestReachabilityContractMatchesSchemaAndDefaultPrompt(t *testing.T) {
	var schema struct {
		Properties struct {
			Findings struct {
				Items struct {
					Properties struct {
						Reachability struct {
							Properties struct {
								Status struct {
									Enum []string `json:"enum"`
								} `json:"status"`
							} `json:"properties"`
						} `json:"reachability"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"findings"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(ReviewSchema, &schema); err != nil {
		t.Fatalf("parse review schema: %v", err)
	}

	want := []string{
		model.ReachabilityDemonstrated,
		model.ReachabilityNotDemonstrated,
		model.ReachabilityNotApplicable,
		model.ReachabilityUncertain,
	}
	got := schema.Properties.Findings.Items.Properties.Reachability.Properties.Status.Enum
	if !slices.Equal(got, want) {
		t.Fatalf("reachability schema statuses = %v, want %v", got, want)
	}
	for _, status := range got {
		if !model.ValidReachabilityStatus(status) {
			t.Errorf("schema status %q is rejected by runtime validation", status)
		}
	}
	if !strings.Contains(DefaultReviewPrompt, "reachability status `not_applicable`") {
		t.Fatalf("default review prompt does not explain not_applicable reachability")
	}
	if !strings.Contains(DefaultReviewPrompt, "Cora-captured web evidence") || !strings.Contains(DefaultReviewPrompt, "evidence ID and SHA-256") {
		t.Fatalf("default review prompt does not constrain external web evidence")
	}
}
