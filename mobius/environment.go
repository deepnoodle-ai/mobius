package mobius

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type EnvironmentGitCredentialRequest struct {
	RepoFullName string `json:"repo_full_name"`
	Operation    string `json:"operation,omitempty"`
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
	if err := c.doJSON(ctx, http.MethodPost, "/v1/projects/"+url.PathEscape(c.projectHandle)+"/environments/"+url.PathEscape(environmentID)+"/git/credentials", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
