package cora

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
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
