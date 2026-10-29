// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Workloads (0.10): a named, stored run request that Wardyn keeps running. A workload is the same
// intent as a `kind: Run` document plus a name; applying an unchanged one does nothing, applying a
// changed one starts a new run and then ends the old. A workload grants nothing: every launch is
// the ordinary create path under the workload's owner.
//
// One document, two headers. `wardyn/v1` `Workload` is the file a person or a pipeline applies; the
// Kubernetes `wardyn.dev/v1alpha1` `WardynWorkload` carries the same fields in `spec`, so a team
// keeps one file for either path. A custom resource's `status` is written by the runner from the
// organisation's answer and is never read back.

// The workload document headers.
const (
	WorkloadDocumentKind = "Workload"
	WorkloadCRAPIVersion = "wardyn.dev/v1alpha1"
	WorkloadCRKind       = "WardynWorkload"
)

// Workload limits. MaxWorkloadSpecBytes is the most a document's intent, with its header fields,
// may take; the organisation and the custom resource's runner hold the same cap.
const (
	MaxWorkloadSpecBytes   = 256 << 10
	MaxWorkloadDescription = 200
	MaxWorkloadRestartKey  = 64
)

// workloadNameRE is a DNS label that starts with a letter: the custom resource's name maps to it 1:1.
// internal/runnerwire holds the same grammar for the names a runner reports.
var workloadNameRE = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)

// WorkloadDocument is one workload, as a file holds it. The same struct reads either header:
// ParseWorkload fills Namespace from a custom resource's metadata (empty for the `wardyn/v1`
// header), and marshalling writes the `wardyn/v1` header. CR writes the other.
type WorkloadDocument struct {
	APIVersion string `json:"api_version"`
	Kind       string `json:"kind"`
	// Name is a DNS label that starts with a letter, so a custom resource's name maps to it 1:1.
	Name string `json:"name"`
	// Description is not part of the digest: changing it applies without a new run.
	Description string `json:"description,omitempty"`
	// RunIn is the unit (id or name) the run is launched in.
	RunIn string `json:"run_in,omitempty"`
	// RestartKey is opaque; changing it starts a fresh run of the same intent.
	RestartKey string `json:"restart_key,omitempty"`
	// Intent is exactly a `kind: Run` document's intent.
	Intent TemplateIntent `json:"intent"`
	// Namespace is the custom resource's namespace; it is no part of the `wardyn/v1` header.
	Namespace string `json:"-"`
}

// WorkloadCR is the `WardynWorkload` custom resource, with only the metadata Wardyn reads. Status
// is what a runner wrote back; it is kept so a file exported from a cluster reads, and is never
// authority.
type WorkloadCR struct {
	APIVersion string             `json:"apiVersion"`
	Kind       string             `json:"kind"`
	Metadata   WorkloadCRMetadata `json:"metadata"`
	Spec       WorkloadCRSpec     `json:"spec"`
	Status     json.RawMessage    `json:"status,omitempty"`
}

// WorkloadCRMetadata is the custom resource's name and namespace.
type WorkloadCRMetadata struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// UnmarshalJSON reads the name and namespace and ignores the rest of the metadata (labels,
// uid, timestamps), which a cluster adds to every object.
func (m *WorkloadCRMetadata) UnmarshalJSON(data []byte) error {
	type plain WorkloadCRMetadata
	return json.Unmarshal(data, (*plain)(m))
}

// WorkloadCRSpec is the custom resource's spec: the document's fields without its name.
type WorkloadCRSpec struct {
	Description string         `json:"description,omitempty"`
	RunIn       string         `json:"run_in,omitempty"`
	RestartKey  string         `json:"restart_key,omitempty"`
	Intent      TemplateIntent `json:"intent"`
}

// ParseWorkload reads one workload from JSON (a YAML file is converted by the caller) under either
// header, and validates it. A field it does not know is an error, never dropped; a custom
// resource's metadata beyond its name and namespace, and its status, are ignored.
func ParseWorkload(raw []byte) (WorkloadDocument, error) {
	var doc WorkloadDocument
	var probe struct {
		APIVersion *string `json:"apiVersion"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return doc, fmt.Errorf("read the workload: %w", err)
	}
	if probe.APIVersion != nil {
		var cr WorkloadCR
		if err := decodeStrict(raw, &cr); err != nil {
			return doc, fmt.Errorf("read the workload: %w", err)
		}
		if cr.APIVersion != WorkloadCRAPIVersion || cr.Kind != WorkloadCRKind {
			return doc, fmt.Errorf("a %s %s is not a workload: want %s %s", cr.APIVersion, cr.Kind, WorkloadCRAPIVersion, WorkloadCRKind)
		}
		doc = WorkloadDocument{APIVersion: TemplateDocumentVersion, Kind: WorkloadDocumentKind, Name: cr.Metadata.Name, Namespace: cr.Metadata.Namespace,
			Description: cr.Spec.Description, RunIn: cr.Spec.RunIn, RestartKey: cr.Spec.RestartKey, Intent: cr.Spec.Intent}
		return doc, doc.Validate()
	}
	if err := decodeStrict(raw, &doc); err != nil {
		return doc, fmt.Errorf("read the workload: %w", err)
	}
	if doc.APIVersion != TemplateDocumentVersion || doc.Kind != WorkloadDocumentKind {
		return doc, fmt.Errorf("a %s %s is not a workload: want %s %s", doc.APIVersion, doc.Kind, TemplateDocumentVersion, WorkloadDocumentKind)
	}
	return doc, doc.Validate()
}

func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// Validate checks what can be known without the organisation: the name, the header fields, and the
// size of the spec. Whether the intent is a valid run is the organisation's door to say.
func (d WorkloadDocument) Validate() error {
	switch {
	case !workloadNameRE.MatchString(d.Name):
		return fmt.Errorf("workload name %q must be a lowercase DNS label starting with a letter and ending with a letter or digit, at most 63 characters", d.Name)
	case utf8.RuneCountInString(d.Description) > MaxWorkloadDescription || hasControl(d.Description):
		return fmt.Errorf("description must be at most %d characters, with no control characters", MaxWorkloadDescription)
	case utf8.RuneCountInString(d.RestartKey) > MaxWorkloadRestartKey || hasControl(d.RestartKey):
		return fmt.Errorf("restart_key must be at most %d characters, with no control characters", MaxWorkloadRestartKey)
	case hasControl(d.RunIn):
		return errors.New("run_in must have no control characters")
	case len(d.Intent.fields) == 0:
		return errors.New("a workload needs an intent")
	}
	spec, err := json.Marshal(d.Request())
	if err != nil {
		return err
	}
	if len(spec) > MaxWorkloadSpecBytes {
		return fmt.Errorf("a workload spec is at most %d bytes", MaxWorkloadSpecBytes)
	}
	return nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// Request is the body PUT /api/v1/workloads/{name} takes.
func (d WorkloadDocument) Request() WorkloadApplyRequest {
	return WorkloadApplyRequest{Description: d.Description, RunIn: d.RunIn, RestartKey: d.RestartKey, Intent: d.Intent}
}

// CR is the document as a custom resource in namespace.
func (d WorkloadDocument) CR(namespace string) WorkloadCR {
	return WorkloadCR{APIVersion: WorkloadCRAPIVersion, Kind: WorkloadCRKind,
		Metadata: WorkloadCRMetadata{Name: d.Name, Namespace: namespace},
		Spec:     WorkloadCRSpec{Description: d.Description, RunIn: d.RunIn, RestartKey: d.RestartKey, Intent: d.Intent}}
}

// WorkloadApplyRequest is the PUT /api/v1/workloads/{name} body: the document without its header
// and name. The name is the path's.
type WorkloadApplyRequest struct {
	Description string         `json:"description,omitempty"`
	RunIn       string         `json:"run_in,omitempty"`
	RestartKey  string         `json:"restart_key,omitempty"`
	Intent      TemplateIntent `json:"intent"`
}

// WorkloadOutcome says what an apply did to the stored workload.
type WorkloadOutcome string

// The apply outcomes.
const (
	WorkloadCreated   WorkloadOutcome = "created"
	WorkloadUpdated   WorkloadOutcome = "updated"
	WorkloadUnchanged WorkloadOutcome = "unchanged"
)

// WorkloadLaunchState says whether an apply or restart started a run.
type WorkloadLaunchState string

// The launch states. Refused is still a 200: the spec was stored, and the previous run keeps
// running.
const (
	WorkloadLaunched WorkloadLaunchState = "launched"
	WorkloadNoLaunch WorkloadLaunchState = "none"
	WorkloadRefused  WorkloadLaunchState = "refused"
)

// WorkloadSource says which writer owns a workload: the API and CLI, or a custom resource.
type WorkloadSource string

// The workload sources.
const (
	WorkloadSourceAPI WorkloadSource = "api"
	WorkloadSourceK8s WorkloadSource = "k8s"
)

// WorkloadLaunch is the launch half of an apply's answer.
type WorkloadLaunch struct {
	State    WorkloadLaunchState `json:"state"`
	RunID    *uuid.UUID          `json:"run_id,omitempty"`
	Reason   string              `json:"reason,omitempty"`
	Message  string              `json:"message,omitempty"`
	Warnings []string            `json:"warnings,omitempty"`
}

// WorkloadRefusal is why the last launch attempt of a generation was refused.
type WorkloadRefusal struct {
	Generation int64     `json:"generation"`
	Reason     string    `json:"reason"`
	Message    string    `json:"message,omitempty"`
	At         time.Time `json:"at"`
}

// Workload is one stored workload as the organisation reports it.
type Workload struct {
	Name        string         `json:"name"`
	Owner       string         `json:"owner"`
	Source      WorkloadSource `json:"source"`
	Namespace   string         `json:"namespace,omitempty"`
	Description string         `json:"description,omitempty"`
	RunIn       string         `json:"run_in,omitempty"`
	RestartKey  string         `json:"restart_key,omitempty"`
	// Intent is the door-normalised intent.
	Intent TemplateIntent `json:"intent"`
	// Digest is the hex SHA-256 over the intent, run_in and restart_key; an apply of the same digest changes
	// nothing. It is withheld from a reader who sees the intent with its host axis withheld.
	Digest string `json:"digest,omitempty"`
	// Generation counts spec changes; CurrentGeneration is the one the current run was launched from.
	Generation        int64      `json:"generation"`
	CurrentGeneration int64      `json:"current_generation"`
	RunID             *uuid.UUID `json:"run_id,omitempty"`
	// State is the current run's state, with its service health when it has one.
	State       string           `json:"state,omitempty"`
	LastRefusal *WorkloadRefusal `json:"last_refusal,omitempty"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
}

// WorkloadApplyResult is the answer of an apply or restart.
type WorkloadApplyResult struct {
	Workload Workload        `json:"workload"`
	Outcome  WorkloadOutcome `json:"outcome"`
	Launch   WorkloadLaunch  `json:"launch"`
}

// WorkloadBinding authorises the custom resources of one namespace to launch runs as a service
// identity on a runner pool.
type WorkloadBinding struct {
	ID        uuid.UUID `json:"id"`
	PoolID    uuid.UUID `json:"pool_id"`
	Namespace string    `json:"namespace"`
	Service   string    `json:"service"`
	// Sponsor is the service identity's sponsor when the binding was made; a new sponsor ends it.
	Sponsor   string    `json:"sponsor"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateWorkloadBindingRequest is the POST /api/v1/workload-bindings body.
type CreateWorkloadBindingRequest struct {
	// Pool is the runner pool's id or name.
	Pool      string `json:"pool"`
	Namespace string `json:"namespace"`
	Service   string `json:"service"`
}

func workloadPath(name, owner string) string {
	p := "/api/v1/workloads/" + url.PathEscape(name)
	if owner != "" {
		p += "?owner=" + url.QueryEscape(owner)
	}
	return p
}

// ApplyWorkload stores the workload and launches it when it is new or changed, for the caller.
// PUT /api/v1/workloads/{name}.
func (c *Client) ApplyWorkload(ctx context.Context, name string, req WorkloadApplyRequest) (WorkloadApplyResult, error) {
	var out WorkloadApplyResult
	err := c.do(ctx, http.MethodPut, workloadPath(name, ""), req, &out)
	return out, err
}

// ListWorkloadsPage is ListWorkloads plus the server's X-Wardyn-Truncated signal: truncated=true
// means a further page exists.
func (c *Client) ListWorkloadsPage(ctx context.Context, owner string, opts ...ListOpts) (workloads []Workload, truncated bool, err error) {
	path := "/api/v1/workloads"
	if owner != "" {
		path += "?owner=" + url.QueryEscape(owner)
	}
	var hdr http.Header
	err = c.do(ctx, http.MethodGet, appendListOpts(path, opts), nil, &workloads, &hdr)
	return workloads, hdr.Get("X-Wardyn-Truncated") == "true", err
}

// ListWorkloads lists the caller's workloads; owner, for an administrator or a service identity's
// sponsor, names another principal. GET /api/v1/workloads. Prefer ListWorkloadsPage, which also
// returns the server's truncation signal.
func (c *Client) ListWorkloads(ctx context.Context, owner string, opts ...ListOpts) ([]Workload, error) {
	workloads, _, err := c.ListWorkloadsPage(ctx, owner, opts...)
	return workloads, err
}

// GetWorkload reads one workload. GET /api/v1/workloads/{name}.
func (c *Client) GetWorkload(ctx context.Context, name, owner string) (Workload, error) {
	var out Workload
	err := c.do(ctx, http.MethodGet, workloadPath(name, owner), nil, &out)
	return out, err
}

// DeleteWorkload ends the workload's run and deletes it. DELETE /api/v1/workloads/{name}.
func (c *Client) DeleteWorkload(ctx context.Context, name, owner string) error {
	return c.do(ctx, http.MethodDelete, workloadPath(name, owner), nil, nil)
}

// RestartWorkload starts a fresh run of the stored spec and then ends the old one.
// POST /api/v1/workloads/{name}/restart.
func (c *Client) RestartWorkload(ctx context.Context, name, owner string) (WorkloadApplyResult, error) {
	var out WorkloadApplyResult
	path := "/api/v1/workloads/" + url.PathEscape(name) + "/restart"
	if owner != "" {
		path += "?owner=" + url.QueryEscape(owner)
	}
	err := c.do(ctx, http.MethodPost, path, nil, &out)
	return out, err
}

// CreateWorkloadBinding binds a namespace on a pool's runners to a service identity (security
// tier, a signed-in person). POST /api/v1/workload-bindings.
func (c *Client) CreateWorkloadBinding(ctx context.Context, req CreateWorkloadBindingRequest) (WorkloadBinding, error) {
	var out WorkloadBinding
	err := c.do(ctx, http.MethodPost, "/api/v1/workload-bindings", req, &out)
	return out, err
}

// ListWorkloadBindingsPage is ListWorkloadBindings plus the server's X-Wardyn-Truncated signal:
// truncated=true means a further page exists.
func (c *Client) ListWorkloadBindingsPage(ctx context.Context, opts ...ListOpts) (bindings []WorkloadBinding, truncated bool, err error) {
	var hdr http.Header
	err = c.do(ctx, http.MethodGet, appendListOpts("/api/v1/workload-bindings", opts), nil, &bindings, &hdr)
	return bindings, hdr.Get("X-Wardyn-Truncated") == "true", err
}

// ListWorkloadBindings lists every binding (security tier). GET /api/v1/workload-bindings.
// Prefer ListWorkloadBindingsPage, which also returns the server's truncation signal.
func (c *Client) ListWorkloadBindings(ctx context.Context, opts ...ListOpts) ([]WorkloadBinding, error) {
	bindings, _, err := c.ListWorkloadBindingsPage(ctx, opts...)
	return bindings, err
}

// DeleteWorkloadBinding removes a binding (security tier); runs it launched continue.
// DELETE /api/v1/workload-bindings/{id}.
func (c *Client) DeleteWorkloadBinding(ctx context.Context, id uuid.UUID) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/workload-bindings/"+id.String(), nil, nil)
}
