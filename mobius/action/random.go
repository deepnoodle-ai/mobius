package action

import (
	"crypto/rand"
	"fmt"
	mathrand "math/rand"
	"strings"
	"time"

	"github.com/deepnoodle-ai/mobius/mobius"
)

// RandomInput defines the input parameters for the random action.
type RandomInput struct {
	Type    string   `json:"type"`    // uuid, number, string, choice, boolean
	Min     float64  `json:"min"`     // minimum value for number generation
	Max     float64  `json:"max"`     // maximum value for number generation
	Length  int      `json:"length"`  // length for string generation
	Choices []string `json:"choices"` // choices for selection
	Count   int      `json:"count"`   // number of items to generate
	Charset string   `json:"charset"` // character set for string generation
	Seed    int64    `json:"seed"`    // seed for reproducible randomness
}

// NewRandomAction returns an action that generates random values
// of various shapes: uuid, number, float, string, choice, boolean,
// alphanumeric, hex.
func NewRandomAction() mobius.Action {
	return mobius.NewTypedAction("random", func(ctx mobius.Context, params RandomInput) (any, error) {
		if params.Type == "" {
			params.Type = "uuid"
		}

		var rng *mathrand.Rand
		if params.Seed != 0 {
			rng = mathrand.New(mathrand.NewSource(params.Seed))
		} else {
			rng = mathrand.New(mathrand.NewSource(time.Now().UnixNano()))
		}

		if params.Count <= 0 {
			params.Count = 1
		}

		values := make([]any, 0, params.Count)
		for i := 0; i < params.Count; i++ {
			value, err := generateRandom(rng, params)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}

		if params.Count == 1 {
			return values[0], nil
		}
		return values, nil
	})
}

func generateRandom(rng *mathrand.Rand, params RandomInput) (any, error) {
	switch strings.ToLower(params.Type) {
	case "uuid", "guid":
		return generateUUID()

	case "number", "int", "integer":
		if params.Max <= params.Min {
			params.Max = params.Min + 100
		}
		minInt := int(params.Min)
		maxInt := int(params.Max)
		return rng.Intn(maxInt-minInt+1) + minInt, nil

	case "float", "decimal":
		if params.Max <= params.Min {
			params.Max = params.Min + 1.0
		}
		return params.Min + rng.Float64()*(params.Max-params.Min), nil

	case "string", "text":
		length := params.Length
		if length <= 0 {
			length = 10
		}
		charset := params.Charset
		if charset == "" {
			charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
		}
		return randomString(rng, length, charset), nil

	case "choice", "select":
		if len(params.Choices) == 0 {
			return nil, fmt.Errorf("choices cannot be empty for choice type")
		}
		return params.Choices[rng.Intn(len(params.Choices))], nil

	case "boolean", "bool":
		return rng.Intn(2) == 1, nil

	case "alphanumeric":
		length := params.Length
		if length <= 0 {
			length = 8
		}
		return randomString(rng, length, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"), nil

	case "hex":
		length := params.Length
		if length <= 0 {
			length = 8
		}
		return randomString(rng, length, "0123456789abcdef"), nil

	default:
		return nil, fmt.Errorf("unsupported type: %s", params.Type)
	}
}

func generateUUID() (string, error) {
	uuid := make([]byte, 16)
	if _, err := rand.Read(uuid); err != nil {
		return "", err
	}
	uuid[6] = (uuid[6] & 0x0f) | 0x40 // Version 4
	uuid[8] = (uuid[8] & 0x3f) | 0x80 // Variant bits
	return fmt.Sprintf("%x-%x-%x-%x-%x", uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:]), nil
}

func randomString(rng *mathrand.Rand, length int, charset string) string {
	result := make([]byte, length)
	for i := range result {
		result[i] = charset[rng.Intn(len(charset))]
	}
	return string(result)
}
