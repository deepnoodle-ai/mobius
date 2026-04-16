package action

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/deepnoodle-ai/mobius/mobius"
)

// JSONInput defines the input parameters for the JSON action.
type JSONInput struct {
	Operation string `json:"operation"`  // parse, stringify, query, merge, validate
	Data      string `json:"data"`       // JSON string to work with
	Query     string `json:"query"`      // JSONPath-like query (simplified)
	MergeWith string `json:"merge_with"` // JSON string to merge with
}

// NewJSONAction returns an action that performs basic JSON
// operations: parse, stringify, query, merge, validate.
func NewJSONAction() mobius.Action {
	return mobius.NewTypedAction("json", func(ctx mobius.Context, params JSONInput) (any, error) {
		if params.Operation == "" {
			params.Operation = "parse"
		}
		switch strings.ToLower(params.Operation) {
		case "parse":
			var result any
			if err := json.Unmarshal([]byte(params.Data), &result); err != nil {
				return nil, err
			}
			return result, nil

		case "stringify":
			var parsed any
			if err := json.Unmarshal([]byte(params.Data), &parsed); err != nil {
				return nil, err
			}
			formatted, err := json.MarshalIndent(parsed, "", "  ")
			if err != nil {
				return nil, err
			}
			return string(formatted), nil

		case "query":
			if params.Query == "" {
				return nil, fmt.Errorf("query cannot be empty for query operation")
			}
			var parsed any
			if err := json.Unmarshal([]byte(params.Data), &parsed); err != nil {
				return nil, err
			}
			return queryJSON(parsed, params.Query)

		case "merge":
			if params.MergeWith == "" {
				return nil, fmt.Errorf("merge_with cannot be empty for merge operation")
			}
			var data1, data2 map[string]any
			if err := json.Unmarshal([]byte(params.Data), &data1); err != nil {
				return nil, fmt.Errorf("failed to parse main data: %v", err)
			}
			if err := json.Unmarshal([]byte(params.MergeWith), &data2); err != nil {
				return nil, fmt.Errorf("failed to parse merge data: %v", err)
			}
			return mergeJSON(data1, data2), nil

		case "validate":
			var parsed any
			if err := json.Unmarshal([]byte(params.Data), &parsed); err != nil {
				return false, nil
			}
			return true, nil

		default:
			return nil, fmt.Errorf("unsupported operation: %s", params.Operation)
		}
	})
}

func queryJSON(data any, query string) (any, error) {
	if query == "" || query == "." {
		return data, nil
	}
	query = strings.TrimPrefix(query, ".")
	parts := strings.Split(query, ".")

	current := data
	for _, part := range parts {
		if part == "" {
			continue
		}
		switch v := current.(type) {
		case map[string]any:
			val, exists := v[part]
			if !exists {
				return nil, fmt.Errorf("key '%s' not found", part)
			}
			current = val
		case []any:
			var idx int
			if _, err := fmt.Sscanf(part, "%d", &idx); err != nil {
				return nil, fmt.Errorf("invalid array index '%s'", part)
			}
			if idx < 0 || idx >= len(v) {
				return nil, fmt.Errorf("array index %d out of bounds", idx)
			}
			current = v[idx]
		default:
			return nil, fmt.Errorf("cannot query into non-object/non-array type")
		}
	}
	return current, nil
}

func mergeJSON(obj1, obj2 map[string]any) map[string]any {
	result := make(map[string]any, len(obj1)+len(obj2))
	for k, v := range obj1 {
		result[k] = v
	}
	for k, v := range obj2 {
		if existing, exists := result[k]; exists {
			if existingMap, ok := existing.(map[string]any); ok {
				if vMap, ok := v.(map[string]any); ok {
					result[k] = mergeJSON(existingMap, vMap)
					continue
				}
			}
		}
		result[k] = v
	}
	return result
}
