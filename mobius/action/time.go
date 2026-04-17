package action

import (
	"time"

	"github.com/deepnoodle-ai/mobius/mobius"
)

// TimeInput defines the input parameters for the time action.
type TimeInput struct {
	UTC bool `json:"utc"`
}

// NewTimeAction returns an action that reports the current time.
func NewTimeAction() mobius.Action {
	return mobius.NewTypedAction("time", func(ctx mobius.Context, params TimeInput) (time.Time, error) {
		if params.UTC {
			return time.Now().UTC(), nil
		}
		return time.Now(), nil
	})
}
