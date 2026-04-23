// Package mobius provides the Go SDK for building Mobius workers and for
// managing workflows, runs, triggers, webhooks, and other Mobius resources.
//
// Workers claim individual jobs — one action invocation on behalf of a
// workflow run — from the Mobius runtime API, execute the corresponding
// registered action locally, and report the result back. The backend
// owns the workflow engine; the SDK only defines and runs actions.
//
// Typical usage:
//
//	client, err := mobius.NewClient(mobius.WithAPIKey("prod/mbx_..."))
//	if err != nil { log.Fatal(err) }
//	worker := client.NewWorker(mobius.WorkerConfig{})
//	mobius.RegisterAction(worker, "send_email", sendEmail)
//	worker.Run(ctx)
package mobius

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Action is one named unit of work a worker can execute on behalf of
// the Mobius runtime. Actions are registered with a Worker and
// invoked by name whenever the server dispatches a matching job.
type Action interface {
	Name() string
	Execute(ctx Context, params map[string]any) (any, error)
}

// ActionFunc adapts a plain function to the Action interface.
func ActionFunc(name string, fn func(ctx Context, params map[string]any) (any, error)) Action {
	return &actionFunc{name: name, fn: fn}
}

type actionFunc struct {
	name string
	fn   func(ctx Context, params map[string]any) (any, error)
}

func (a *actionFunc) Name() string { return a.name }
func (a *actionFunc) Execute(ctx Context, params map[string]any) (any, error) {
	return a.fn(ctx, params)
}

// NewTypedAction wraps a strongly-typed function as an Action. The
// incoming parameters map is marshalled through JSON into TParams before
// the function is called.
func NewTypedAction[TParams, TResult any](name string, fn func(ctx Context, params TParams) (TResult, error)) Action {
	return &typedAction[TParams, TResult]{name: name, fn: fn}
}

type typedAction[TParams, TResult any] struct {
	name string
	fn   func(ctx Context, params TParams) (TResult, error)
}

func (a *typedAction[TParams, TResult]) Name() string { return a.name }

func (a *typedAction[TParams, TResult]) Execute(ctx Context, params map[string]any) (any, error) {
	var typed TParams
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("mobius: invalid parameters for action %q: %w", a.name, err)
	}
	if err := json.Unmarshal(raw, &typed); err != nil {
		return nil, fmt.Errorf("mobius: invalid parameters for action %q: %w", a.name, err)
	}
	return a.fn(ctx, typed)
}

// ErrDuplicateAction is returned when Register is called with an
// action name that has already been registered.
var ErrDuplicateAction = errors.New("mobius: duplicate action registration")

// ActionRegistry owns the set of actions a Worker can execute by
// name. It is safe for concurrent use.
type ActionRegistry struct {
	mu      sync.RWMutex
	actions map[string]Action
}

// NewActionRegistry returns an empty registry.
func NewActionRegistry() *ActionRegistry {
	return &ActionRegistry{actions: map[string]Action{}}
}

// Register adds an action to the registry.
func (r *ActionRegistry) Register(a Action) error {
	if a == nil {
		return errors.New("mobius: nil action")
	}
	name := a.Name()
	if name == "" {
		return errors.New("mobius: action has empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.actions[name]; exists {
		return fmt.Errorf("%w: %q", ErrDuplicateAction, name)
	}
	r.actions[name] = a
	return nil
}

// MustRegister panics on registration failure.
func (r *ActionRegistry) MustRegister(a Action) {
	if err := r.Register(a); err != nil {
		panic(err)
	}
}

// Get returns the action registered under name, if any.
func (r *ActionRegistry) Get(name string) (Action, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.actions[name]
	return a, ok
}

// Names returns the registered action names in sorted order.
func (r *ActionRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.actions))
	for n := range r.actions {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// RegisterAction registers a typed action function with a Worker.
// P is the parameter type and R is the return type.
func RegisterAction[P, R any](w *Worker, name string, fn func(ctx Context, params P) (R, error)) {
	w.registry.MustRegister(NewTypedAction(name, fn))
}

// Register attaches a pre-constructed Action to the Worker. Use this
// for actions that need constructor arguments or for actions from
// the mobius/actions package. For plain typed functions, prefer
// RegisterAction.
func (w *Worker) Register(a Action) {
	w.registry.MustRegister(a)
}
