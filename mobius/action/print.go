// Package action provides a small set of general-purpose actions
// that workers can register out of the box: print, fail, json, time, and
// random. They cover most trivial and test workflows without requiring
// custom code.
package action

import (
	"fmt"
	"io"
	"os"

	"github.com/deepnoodle-ai/mobius/mobius"
)

// PrintInput defines the input parameters for the print action.
type PrintInput struct {
	Message string `json:"message"`
	Args    []any  `json:"args"`
}

// NewPrintAction returns a print action that writes to os.Stdout.
func NewPrintAction() mobius.Action {
	return NewPrintActionTo(os.Stdout)
}

// NewPrintActionTo returns a print action that writes to w. If w is
// nil, os.Stdout is used.
func NewPrintActionTo(w io.Writer) mobius.Action {
	if w == nil {
		w = os.Stdout
	}
	return mobius.NewTypedAction("print", func(ctx mobius.Context, params PrintInput) (string, error) {
		message := fmt.Sprintf(params.Message, params.Args...)
		fmt.Fprintln(w, message)
		return message, nil
	})
}
