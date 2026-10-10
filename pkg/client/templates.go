// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Run templates (0.9): a reusable run setup, whole or partial, that a person
// keeps for themselves (person scope), an organisation administrator
// publishes (org scope) or a group's administrator publishes to that group
// (group scope). These shapes are the contract the template store fills in:
// until it lands, the routes answer 501 templates_unavailable, and the typed
// methods below make the real calls and return that error.
//
// A template is content. It is never an admission: using one re-checks the
// launcher's current policy, credentials, components, pools and drives, and a
// published template grants none of them. It carries no secret value, session
// or claim state, run identity, or historical admission, and a document that
// does is refused, not trimmed. It carries no title or description either: a
// run is named when it is launched.

// TemplateDocumentVersion and TemplateDocumentKind identify the document
// schema an import or a saved template carries.
const (
	TemplateDocumentVersion = "wardyn/v1"
	TemplateDocumentKind    = "RunTemplate"
)

// TemplateScope says who a template is for.
type TemplateScope = types.TemplateScope

// The template scopes.
const (
	TemplateScopePerson = types.TemplateScopePerson
	TemplateScopeOrg    = types.TemplateScopeOrg
	TemplateScopeGroup  = types.TemplateScopeGroup
)

// TemplateGroupAdmin is a bounded grant letting one person manage one group's
// templates; an organisation administrator writes it.
type TemplateGroupAdmin = types.TemplateGroupAdmin

// TemplateCoverage says what a template intends to hold. Full is a whole run
// setup: nothing in it is waiting on a later choice. Partial is a mix of
// parts, and may list what it needs set up when it is used.
type TemplateCoverage string

// The template coverages.
const (
	TemplateCoverageFull    TemplateCoverage = "full"
	TemplateCoveragePartial TemplateCoverage = "partial"
)

// TemplateFormat is the text an import is written in.
type TemplateFormat string

// The import formats.
const (
	TemplateFormatJSON TemplateFormat = "json"
	TemplateFormatYAML TemplateFormat = "yaml"
)

// TemplatePart is one group the refinement flow lets a person keep or leave
// out, and one entry of a template's Includes.
type TemplatePart string

// The template parts.
const (
	TemplatePartRunner       TemplatePart = "runner"
	TemplatePartResources    TemplatePart = "resources"
	TemplatePartRepositories TemplatePart = "repositories"
	TemplatePartDrives       TemplatePart = "drives"
	TemplatePartToolsImage   TemplatePart = "tools_image"
	TemplatePartEgress       TemplatePart = "egress"
	TemplatePartCredentials  TemplatePart = "credentials"
	TemplatePartComponents   TemplatePart = "components"
	TemplatePartAccessRules  TemplatePart = "access_rules"
	TemplatePartPolicyRef    TemplatePart = "policy_ref"
)

// TemplateIntent is the reusable part of a run request: the fields the
// template says something about, and no others. It keeps PRESENCE. A field
// that is absent means "this template does not specify it"; a field that is
// present and false, zero, empty or an empty list is a choice. The Go zero
// values of CreateRunRequest cannot tell those apart, so the intent holds each
// field's JSON as written and Request materialises the present ones. Null is
// never a value.
//
// Its fields are the canonical request fields (CreateRunRequest) that a
// template may carry, with inline_policy as a partial policy spec: the server
// lays only the keys present onto a readable source policy, and a key left out
// keeps the source's value. Request is a shape check, not that overlay.
type TemplateIntent struct {
	fields map[string]json.RawMessage
}

// Names are the fields the intent specifies, sorted.
func (i TemplateIntent) Names() []string {
	names := make([]string, 0, len(i.fields))
	for name := range i.fields {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Has reports whether the intent specifies the field, whatever its value.
func (i TemplateIntent) Has(name string) bool { _, ok := i.fields[name]; return ok }

// Raw is the field's JSON as stored, nil when absent.
func (i TemplateIntent) Raw(name string) json.RawMessage { return slices.Clone(i.fields[name]) }

// Set specifies one field, replacing any earlier value.
func (i *TemplateIntent) Set(name string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if i.fields == nil {
		i.fields = map[string]json.RawMessage{}
	}
	i.fields[name] = raw
	return nil
}

// Unset removes a field: the template stops specifying it.
func (i *TemplateIntent) Unset(name string) { delete(i.fields, name) }

// Request materialises the present fields into a request, for shape checks. A
// request cannot tell an absent field from an empty one, and a request's
// inline_policy replaces its source policy, so this is not how a template is
// applied: the server overlays the present policy keys onto the source policy.
// A field the request does not know is an error, never dropped.
func (i TemplateIntent) Request() (CreateRunRequest, error) {
	var req CreateRunRequest
	body, err := json.Marshal(i.fields)
	if err != nil {
		return req, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	return req, dec.Decode(&req)
}

// MarshalJSON writes the fields in sorted order, each as stored.
func (i TemplateIntent) MarshalJSON() ([]byte, error) {
	if i.fields == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(i.fields)
}

// UnmarshalJSON reads the intent's fields in canonical form: compact, with
// every object's keys sorted, so the same document written as YAML or as JSON,
// in any key order, holds the same bytes (revisions and diffs stay stable).
// Whether each is a field a template may carry is the server's decoder's to say.
func (i *TemplateIntent) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	i.fields = make(map[string]json.RawMessage, len(fields))
	for name, raw := range fields {
		canonical, err := canonicalJSON(raw)
		if err != nil {
			return err
		}
		i.fields[name] = canonical
	}
	return nil
}

// canonicalJSON rewrites one JSON value compactly with sorted object keys,
// numbers as written and no HTML escaping.
func canonicalJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// TemplateSetupNeed names a field the template leaves out but the fields it
// does carry depend on, so the person who uses it knows to set it up.
type TemplateSetupNeed struct {
	// Field is an intent field, or inline_policy.<field>, that the intent does not specify.
	Field  string `json:"field"`
	Reason string `json:"reason,omitempty"`
}

// TemplateDocument is the import and export document of one template (YAML or
// JSON). It carries content only. Owner, scope, group, revision and authorship
// belong to the catalogue and come from the authenticated request; a document
// that names them is refused.
type TemplateDocument struct {
	APIVersion  string           `json:"api_version"`
	Kind        string           `json:"kind"`
	Name        string           `json:"name,omitempty"`
	Description string           `json:"description,omitempty"`
	Coverage    TemplateCoverage `json:"coverage"`
	Intent      TemplateIntent   `json:"intent"`
	// NeedsSetup lists what a partial template leaves for use time. A full
	// template lists none.
	NeedsSetup []TemplateSetupNeed `json:"needs_setup,omitempty"`
}

// TemplateRef pins a template revision in a new-run draft. A later edit of
// the template does not change a draft that holds an earlier revision;
// refreshing it is an explicit draft action.
type TemplateRef struct {
	ID       uuid.UUID `json:"id"`
	Revision int       `json:"revision"`
}

// Template is one catalogue entry, as the server returns it.
type Template struct {
	ID    uuid.UUID     `json:"id"`
	Scope TemplateScope `json:"scope"`
	// OwnerID is the owning person's subject, for person scope.
	OwnerID string `json:"owner_id,omitempty"`
	// GroupID is the group's canonical subject, for group scope.
	GroupID     string           `json:"group_id,omitempty"`
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Revision    int              `json:"revision"`
	Coverage    TemplateCoverage `json:"coverage"`
	Document    TemplateDocument `json:"document"`
	// Writable says whether the caller may update or delete it.
	Writable  bool      `json:"writable"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	CreatedBy string    `json:"created_by,omitempty"`
	UpdatedBy string    `json:"updated_by,omitempty"`
}

// TemplateSummary is one row of the picker: what the template is and what it
// includes, without the document.
type TemplateSummary struct {
	ID          uuid.UUID        `json:"id"`
	Scope       TemplateScope    `json:"scope"`
	GroupID     string           `json:"group_id,omitempty"`
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Revision    int              `json:"revision"`
	Coverage    TemplateCoverage `json:"coverage"`
	Includes    []TemplatePart   `json:"includes"`
	NeedsSetup  []string         `json:"needs_setup"`
	Writable    bool             `json:"writable"`
	UpdatedAt   time.Time        `json:"updated_at"`
}

// TemplateList is GET /api/v1/templates: the templates the caller may use.
type TemplateList struct {
	Templates []TemplateSummary `json:"templates"`
}

// TemplateSaveRequest creates a template, or updates one when ExpectedRevision
// names the revision the caller read. An update against any other revision is
// a conflict. Scope and GroupID say where it is published; the server checks
// the caller's authority for exactly that scope.
type TemplateSaveRequest struct {
	Scope            TemplateScope    `json:"scope"`
	GroupID          string           `json:"group_id,omitempty"`
	Name             string           `json:"name"`
	Description      string           `json:"description,omitempty"`
	Document         TemplateDocument `json:"document"`
	ExpectedRevision *int             `json:"expected_revision,omitempty"`
}

// TemplateImportRequest asks the server to read an import document and say
// whether it can become a draft. It stores nothing.
type TemplateImportRequest struct {
	Format TemplateFormat `json:"format"`
	Source string         `json:"source"`
}

// TemplateDiagnostic is one problem with a document: where, why (a closed
// reason) and a sentence. Path is a JSON path such as intent.workspaces[1].target.
type TemplateDiagnostic struct {
	Path    string `json:"path"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// TemplateImportResult is POST /api/v1/templates/import's answer. Document is
// the normalised document and is set only when Diagnostics is empty; with
// diagnostics the caller keeps its own text and shows them.
type TemplateImportResult struct {
	Document    *TemplateDocument    `json:"document,omitempty"`
	Diagnostics []TemplateDiagnostic `json:"diagnostics"`
}

// TemplateCopyRequest copies (publishes) a template into another scope. The
// caller needs to read the source and to write the target scope; the server
// re-checks every reference the copy would disclose.
type TemplateCopyRequest struct {
	Scope   TemplateScope `json:"scope"`
	GroupID string        `json:"group_id,omitempty"`
	Name    string        `json:"name,omitempty"`
}

// TemplateGroupAdmins is GET /api/v1/admin/template-group-admins.
type TemplateGroupAdmins struct {
	Grants []TemplateGroupAdmin `json:"grants"`
}

// ListTemplates returns the templates the caller may use.
func (c *Client) ListTemplates(ctx context.Context) (TemplateList, error) {
	var out TemplateList
	err := c.do(ctx, http.MethodGet, "/api/v1/templates", nil, &out)
	return out, err
}

// GetTemplate returns one template by id: revision 0 is the current one.
func (c *Client) GetTemplate(ctx context.Context, id uuid.UUID, revision int) (Template, error) {
	path := "/api/v1/templates/" + id.String()
	if revision > 0 {
		path += "/revisions/" + strconv.Itoa(revision)
	}
	var out Template
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// SaveTemplate creates a template when id is uuid.Nil, and updates that
// template otherwise (req.ExpectedRevision is then required by the server).
func (c *Client) SaveTemplate(ctx context.Context, id uuid.UUID, req TemplateSaveRequest) (Template, error) {
	method, path := http.MethodPost, "/api/v1/templates"
	if id != uuid.Nil {
		method, path = http.MethodPut, path+"/"+id.String()
	}
	var out Template
	err := c.do(ctx, method, path, req, &out)
	return out, err
}

// DeleteTemplate removes a template the caller may write.
func (c *Client) DeleteTemplate(ctx context.Context, id uuid.UUID) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/templates/"+id.String(), nil, nil)
}

// CopyTemplate copies a template into another scope and returns the copy.
func (c *Client) CopyTemplate(ctx context.Context, id uuid.UUID, req TemplateCopyRequest) (Template, error) {
	var out Template
	err := c.do(ctx, http.MethodPost, "/api/v1/templates/"+id.String()+"/copy", req, &out)
	return out, err
}

// ImportTemplate asks the server to read an import document. It stores nothing.
func (c *Client) ImportTemplate(ctx context.Context, req TemplateImportRequest) (TemplateImportResult, error) {
	var out TemplateImportResult
	err := c.do(ctx, http.MethodPost, "/api/v1/templates/import", req, &out)
	return out, err
}

// ListTemplateGroupAdmins returns every template-management grant. Admin only.
func (c *Client) ListTemplateGroupAdmins(ctx context.Context) (TemplateGroupAdmins, error) {
	var out TemplateGroupAdmins
	err := c.do(ctx, http.MethodGet, "/api/v1/admin/template-group-admins", nil, &out)
	return out, err
}

// GrantTemplateGroupAdmin lets one person manage one group's templates. Admin
// only; the grant reaches nothing else.
func (c *Client) GrantTemplateGroupAdmin(ctx context.Context, grant TemplateGroupAdmin) (TemplateGroupAdmin, error) {
	var out TemplateGroupAdmin
	err := c.do(ctx, http.MethodPut, "/api/v1/admin/template-group-admins", grant, &out)
	return out, err
}

// RevokeTemplateGroupAdmin removes the grant of the group and person named.
func (c *Client) RevokeTemplateGroupAdmin(ctx context.Context, grant TemplateGroupAdmin) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/admin/template-group-admins", grant, nil)
}

// The custom-component configuration schema (internal/types owns it): the
// declarative fields a component author exposes so the wizard can render the
// component's settings. Types only; a component row carries a schema once the
// components lane adds it.
type (
	ComponentConfigSchema = types.ComponentConfigSchema
	ConfigGroup           = types.ConfigGroup
	ConfigField           = types.ConfigField
	ConfigOption          = types.ConfigOption
	ConfigCondition       = types.ConfigCondition
	ConfigBinding         = types.ConfigBinding
	ComponentConfigValues = types.ComponentConfigValues
	ConfigIssue           = types.ConfigIssue
	ConfigFieldKind       = types.ConfigFieldKind
	ConfigBindTarget      = types.ConfigBindTarget
)

// The schema's closed field kinds and binding targets.
const (
	ConfigKindString     = types.ConfigKindString
	ConfigKindBoolean    = types.ConfigKindBoolean
	ConfigKindInteger    = types.ConfigKindInteger
	ConfigKindNumber     = types.ConfigKindNumber
	ConfigKindEnum       = types.ConfigKindEnum
	ConfigKindStringList = types.ConfigKindStringList
	ConfigKindSecretRef  = types.ConfigKindSecretRef
	ConfigBindConfig     = types.ConfigBindConfig
	ConfigBindSecret     = types.ConfigBindSecret
)
