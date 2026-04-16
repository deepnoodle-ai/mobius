package action

import (
	"fmt"

	"github.com/deepnoodle-ai/mobius/mobius"
)

// FailInput defines the input parameters for the fail action.
type FailInput struct {
	Message string `json:"message"`
}

// FailOutput is the (empty) output of the fail action.
type FailOutput struct{}

// NewFailAction returns an action that always fails. Useful for
// exercising retry and error paths in workflow definitions.
func NewFailAction() mobius.Action {
	return mobius.NewTypedAction("fail", func(ctx mobius.Context, params FailInput) (FailOutput, error) {
		message := params.Message
		if message == "" {
			message = "intentional failure for testing"
		}
		return FailOutput{}, fmt.Errorf("fail action: %s", message)
	})
}
