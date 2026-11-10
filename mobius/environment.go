package mobius

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type EnvironmentGitCredentialRequest struct {
	RepoFullName string `json:"repo_full_name"`
	Operation    string `json:"operation,omitempty"`
	LeaseToken   string `json:"lease_token,omitempty"`
}

type EnvironmentGitCredential struct {
	Host         string    `json:"host"`
	Username     string    `json:"username"`
	Token        string    `json:"token"`
	ExpiresAt    time.Time `json:"expires_at"`
	RepoFullName string    `json:"repo_full_name"`
}

func (c *Client) CreateEnvironmentGitCredential(ctx context.Context, environmentID string, req EnvironmentGitCredentialRequest) (*EnvironmentGitCredential, error) {
	if c == nil {
		return nil, fmt.Errorf("mobius: nil client")
	}
	if strings.TrimSpace(environmentID) == "" {
		return nil, fmt.Errorf("mobius: environment_id is required")
	}
	if strings.TrimSpace(req.RepoFullName) == "" {
		return nil, fmt.Errorf("mobius: repo_full_name is required")
	}
	var out EnvironmentGitCredential
	if err := c.doJSON(ctx, http.MethodPost, "/v1/environments/"+url.PathEscape(environmentID)+"/git/credentials", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ScriptCallRequest is one action call from a workflow script step.
type ScriptCallRequest struct {
	Action string         `json:"action"`
	Input  map[string]any `json:"input,omitempty"`
	Key    string         `json:"key,omitempty"`
}

// ScriptCallResult is the result of one script call. Status is "completed"
// with Output, or "failed" with Error.
type ScriptCallResult struct {
	CallID string           `json:"call_id"`
	Status string           `json:"status"`
	Output any              `json:"output,omitempty"`
	Error  *ScriptCallError `json:"error,omitempty"`
}

// ScriptCallError says why a script call failed or was refused.
type ScriptCallError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ScriptActionDescription describes an action a workflow step can run.
type ScriptActionDescription struct {
	Name         string              `json:"name"`
	Title        string              `json:"title"`
	Description  string              `json:"description"`
	Effect       string              `json:"effect"`
	Inputs       []ScriptActionInput `json:"inputs"`
	InputSchema  map[string]any      `json:"input_schema"`
	OutputSchema map[string]any      `json:"output_schema"`
}

// ScriptActionInput is one top-level input of an action.
type ScriptActionInput struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
	Description string `json:"description"`
}

// scriptCallTimeout bounds one script call. Mobius waits at most five
// minutes for the action, so this leaves room for the round trip.
const scriptCallTimeout = 6 * time.Minute

// CreateScriptCall runs one action for the workflow script step whose job
// holds leaseToken. Only an environment's own worker can call it, for a job
// of that environment.
func (c *Client) CreateScriptCall(ctx context.Context, environmentID, leaseToken string, req ScriptCallRequest) (*ScriptCallResult, error) {
	if c == nil {
		return nil, fmt.Errorf("mobius: nil client")
	}
	if strings.TrimSpace(environmentID) == "" || strings.TrimSpace(leaseToken) == "" {
		return nil, fmt.Errorf("mobius: environment_id and lease token are required")
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, scriptCallTimeout)
	defer cancel()
	var out ScriptCallResult
	path := "/v1/environments/" + url.PathEscape(environmentID) + "/script-calls"
	headers := map[string]string{"X-Mobius-Lease-Token": leaseToken}
	if err := c.doWithHeaders(ctx, c.transferClient, http.MethodPost, path, "application/json", bytes.NewReader(raw), headers, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DescribeScriptAction describes an action for a job of the environment.
func (c *Client) DescribeScriptAction(ctx context.Context, environmentID, leaseToken, action string) (*ScriptActionDescription, error) {
	if c == nil {
		return nil, fmt.Errorf("mobius: nil client")
	}
	if strings.TrimSpace(environmentID) == "" || strings.TrimSpace(leaseToken) == "" {
		return nil, fmt.Errorf("mobius: environment_id and lease token are required")
	}
	var out ScriptActionDescription
	path := "/v1/environments/" + url.PathEscape(environmentID) + "/script-actions/" + url.PathEscape(action)
	headers := map[string]string{"X-Mobius-Lease-Token": leaseToken}
	if err := c.doWithHeaders(ctx, c.httpClient, http.MethodGet, path, "", nil, headers, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
