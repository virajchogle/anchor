// Package ccloud wraps the CockroachDB Cloud CLI as Anchor's action surface.
//
// The CLI is invoked as a subprocess rather than calling the Cloud API over HTTP
// directly. That is deliberate: ccloud is the supported operator interface, it
// carries its own auth handling, and using it means the actions Anchor takes are
// exactly the actions a human on-call engineer would take. The audit trail the
// verifiers read is the same one an operator would read.
package ccloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Client runs ccloud commands.
//
// Authentication comes from an existing `ccloud auth login` session, not from a
// value this struct holds. The CLI binary exposes only CCLOUD_PROFILE and
// CCLOUD_SERVER as environment variables; there is no documented API-key
// variable, so a service account key cannot drive the CLI non-interactively.
//
// That has a deployment consequence worth stating plainly: the Lambda path
// cannot shell out to ccloud. It must call the Cloud API over HTTPS with the
// service account key as a bearer token. The CLI remains the local and
// demonstration action surface, which is also the surface a human operator uses.
type Client struct {
	// Binary is the ccloud executable. Empty means "ccloud" on PATH.
	Binary string
	// Profile selects a named ccloud profile, for running against more than one
	// organization. Empty uses the default.
	Profile string
	// Timeout bounds a single CLI invocation.
	Timeout time.Duration
}

func New() *Client {
	return &Client{Binary: "ccloud", Timeout: 90 * time.Second}
}

func (c *Client) binary() string {
	if c.Binary == "" {
		return "ccloud"
	}
	return c.Binary
}

// run executes a ccloud subcommand and returns stdout.
//
// It always requests JSON. Parsing the human-readable table format would break
// the first time Cockroach Labs adjusts a column, and a verifier that
// misparses is worse than one that errors, because it produces a confident
// wrong answer about whether an action happened.
func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	full := append([]string{}, args...)
	full = append(full, "-o", "json", "--quiet")

	cmd := exec.CommandContext(ctx, c.binary(), full...)
	if c.Profile != "" {
		cmd.Env = append(cmd.Environ(), "CCLOUD_PROFILE="+c.Profile)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, &CLIError{
			Args:   args,
			Stderr: strings.TrimSpace(stderr.String()),
			Err:    err,
		}
	}
	return stdout.Bytes(), nil
}

// CLIError carries enough context to tell a permissions problem apart from a
// genuine "this did not happen", which matters because the two lead to
// different verdicts.
type CLIError struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *CLIError) Error() string {
	return fmt.Sprintf("ccloud %s: %v: %s", strings.Join(e.Args, " "), e.Err, e.Stderr)
}
func (e *CLIError) Unwrap() error { return e.Err }

// SQLUser is a cluster SQL user as reported by ccloud.
type SQLUser struct {
	Name string `json:"name"`
}

// ListSQLUsers returns the SQL users on a cluster.
func (c *Client) ListSQLUsers(ctx context.Context, clusterID string) ([]SQLUser, error) {
	out, err := c.run(ctx, "cluster", "user", "list", clusterID)
	if err != nil {
		return nil, err
	}
	return decodeList[SQLUser](out, "users")
}

// CreateSQLUser creates a SQL user. This is a genuinely non-idempotent
// operation: creating the same user twice is an error, not a no-op.
func (c *Client) CreateSQLUser(ctx context.Context, clusterID, name, password string) error {
	_, err := c.run(ctx, "cluster", "user", "create", clusterID, name, "--password", password)
	return err
}

// DeleteSQLUser removes a SQL user, used to clean up after demo runs.
func (c *Client) DeleteSQLUser(ctx context.Context, clusterID, name string) error {
	_, err := c.run(ctx, "cluster", "user", "delete", clusterID, name)
	return err
}

// AuditEntry is one row of the organization audit log. Field names are
// confirmed against a live organization on 2026-08-11, not guessed.
//
// This type is the foundation of honest verification. Every field here answers
// part of "did MY action happen":
//
//	Action     what was done, e.g. AUDIT_LOG_ACTION_CREATE_SQL_USER
//	ClusterID  which resource it was done to
//	Payload    the operation's own arguments, including identifiers WE chose
//	ID         a unique identifier for this operation, used as external_ref
//	Source     AUDIT_LOG_SOURCE_CLI vs _UI, which separates the agent from a
//	           human working in the console
//	CreatedAt  for windowing the search around the intent's own timestamp
type AuditEntry struct {
	ID          string    `json:"id"`
	Action      string    `json:"action"`
	ClusterID   string    `json:"cluster_id"`
	ClusterName string    `json:"cluster_name"`
	CreatedAt   time.Time `json:"created_at"`
	UserEmail   string    `json:"user_email"`
	ServiceAcct string    `json:"service_account_name"`
	Source      string    `json:"source"`
	TraceID     string    `json:"trace_id"`
	Error       string    `json:"error"`

	// Payload arrives as a JSON-encoded string, not a nested object, so it needs
	// a second unmarshal. Decoding it is what recovers the identifier we chose.
	Payload string `json:"payload"`
}

// Audit action constants, taken from the enum compiled into the ccloud binary.
const (
	ActionCreateSQLUser = "AUDIT_LOG_ACTION_CREATE_SQL_USER"
	ActionDeleteSQLUser = "AUDIT_LOG_ACTION_DELETE_SQL_USER"
	ActionUpdateCluster = "AUDIT_LOG_ACTION_UPDATE_CLUSTER"
	ActionCreateCluster = "AUDIT_LOG_ACTION_CREATE_CLUSTER"

	// EvidenceNone marks a verdict that found no attributable evidence at all.
	EvidenceNone = "none"

	// SourceCLI marks an action taken through the CLI, which is how Anchor acts.
	SourceCLI = "AUDIT_LOG_SOURCE_CLI"
	// SourceUI marks an action taken by a human in the web console.
	SourceUI = "AUDIT_LOG_SOURCE_UI"
)

// PayloadField extracts one string field from the encoded payload.
//
// A decode failure returns an error rather than an empty string, because an
// empty string would compare equal to nothing and quietly produce a NotApplied
// verdict for an action that may well have happened.
func (e AuditEntry) PayloadField(name string) (string, error) {
	if e.Payload == "" {
		return "", fmt.Errorf("ccloud: audit entry %s has an empty payload", e.ID)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(e.Payload), &fields); err != nil {
		return "", fmt.Errorf("ccloud: decoding audit payload for %s: %w", e.ID, err)
	}
	v, ok := fields[name]
	if !ok {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("ccloud: audit payload field %q is %T, not a string", name, v)
	}
	return s, nil
}

// AuditSince returns audit entries recorded at or after a time.
//
// This is the strongest evidence source available for verification, because it
// records what the Cloud control plane actually did rather than what the world
// currently looks like.
func (c *Client) AuditSince(ctx context.Context, since time.Time, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 200
	}
	out, err := c.run(ctx, "audit", "list",
		"--starting-from", since.UTC().Format("2006-01-02T15:04:05Z"),
		"--limit", fmt.Sprintf("%d", limit))
	if err != nil {
		return nil, err
	}
	return decodeList[AuditEntry](out, "entries")
}

// Cluster is the subset of cluster info the verifiers read.
type Cluster struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
	Plan  string `json:"plan"`
}

func (c *Client) ClusterInfo(ctx context.Context, clusterID string) (*Cluster, error) {
	out, err := c.run(ctx, "cluster", "info", clusterID)
	if err != nil {
		return nil, err
	}
	var cl Cluster
	if err := json.Unmarshal(out, &cl); err != nil {
		return nil, fmt.Errorf("ccloud: decoding cluster info: %w", err)
	}
	return &cl, nil
}

// Whoami confirms the credentials work, used as a startup health check so a
// misconfigured key fails at boot rather than mid-incident.
func (c *Client) Whoami(ctx context.Context) error {
	_, err := c.run(ctx, "cluster", "list")
	return err
}

// decodeList tolerates both a bare JSON array and an object wrapping the array
// under a named key, because ccloud is not consistent between subcommands and
// the exact shape is confirmed per command against a live organization.
func decodeList[T any](out []byte, key string) ([]T, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, nil
	}

	if trimmed[0] == '[' {
		var list []T
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return nil, fmt.Errorf("ccloud: decoding array: %w", err)
		}
		return list, nil
	}

	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &wrapper); err != nil {
		return nil, fmt.Errorf("ccloud: decoding object: %w", err)
	}
	raw, ok := wrapper[key]
	if !ok {
		for _, v := range wrapper {
			if len(v) > 0 && v[0] == '[' {
				raw = v
				ok = true
				break
			}
		}
	}
	if !ok {
		return nil, fmt.Errorf("ccloud: no array found under %q in response", key)
	}

	var list []T
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("ccloud: decoding %q: %w", key, err)
	}
	return list, nil
}
