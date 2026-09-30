// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The runPolicySource.Kind values. unknown is a run whose launch path records
// no source: scan, verify, record, probe and login runs write their own
// run.create rows.
const (
	policyKindStored  = "stored"
	policyKindInline  = "inline"
	policyKindDefault = "default"
	policyKindProfile = "profile"
	policyKindUnknown = "unknown"
)

// policyOrigin is what resolvePolicy knows about where its spec came from.
type policyOrigin struct {
	kind, name string
	updatedAt  *time.Time
}

// policySourceRecord is the run.create row's policy_source datum: the policy a
// run STARTED from, before the env-secret posture, the member clamp and every
// launch-time widening changed it. Saved policies are overwritten in place and
// an inline policy leaves only counts, so this row is the only place the
// starting point survives.
type policySourceRecord struct {
	Kind      string              `json:"kind"`
	PolicyID  *uuid.UUID          `json:"policy_id,omitempty"`
	Name      string              `json:"name,omitempty"`
	UpdatedAt *time.Time          `json:"updated_at,omitempty"`
	Bounded   bool                `json:"bounded"` // the member bound applied; gates cause "limits"
	Spec      types.RunPolicySpec `json:"spec"`
}

// newPolicySourceRecord is the one place a starting policy becomes an audit
// datum, and it stamps the spec REDACTED. The run's creator reads run.create
// raw through the audit API, and src is the pre-clamp, admin-authored source:
// unredacted, a member who selected a stored policy could read the mount host
// paths and secret names their clamp removed, which GET /policies/{id} hides
// from them.
func newPolicySourceRecord(kind string, id *uuid.UUID, name string, updatedAt *time.Time, bounded bool, src types.RunPolicySpec) policySourceRecord {
	return policySourceRecord{
		Kind: kind, PolicyID: id, Name: name, UpdatedAt: updatedAt, Bounded: bounded,
		Spec: redactSpecForUser(auditablePolicy(src)),
	}
}

// runPolicyResponse is GET /api/v1/runs/{id}/policy. pkg/client carries its own
// copy (RunPolicyView), pinned to this struct by response_parity_test.go.
type runPolicyResponse struct {
	RunID uuid.UUID `json:"run_id"`
	// State is recorded, not_yet (not terminal, no envelope yet) or never
	// (terminal, no envelope: the run stopped before its sandbox was set up).
	State      string          `json:"state"`
	RecordedAt *time.Time      `json:"recorded_at,omitempty"`
	Source     runPolicySource `json:"source"`
	// Spec strict-decodes as a policy, with restart denies folded in. The disk
	// provenance bit stays outside it, and llm_inspection's secret values are dropped.
	Spec     *types.RunPolicySpec `json:"spec,omitempty"`
	Redacted bool                 `json:"redacted"` // some values are hidden from this reader
	Changes  []runPolicyChange    `json:"changes"`  // never null
	Complete bool                 `json:"complete"` // policy_source was recorded
	// StoredPolicyNow is set for a saved-policy source only.
	StoredPolicyNow *storedPolicyNow `json:"stored_policy_now,omitempty"`
}

type runPolicySource struct {
	Kind          string     `json:"kind"`
	PolicyID      *uuid.UUID `json:"policy_id,omitempty"`
	Name          string     `json:"name,omitempty"`
	Deleted       bool       `json:"deleted,omitempty"`
	Preset        string     `json:"preset,omitempty"`
	PresetVersion int        `json:"preset_version,omitempty"`
}

// runPolicyChange is one group of entries launch added or removed for one
// cause. Entries are redaction-safe keys: a grant is "kind:host" or
// "kind:repo,repo", a mount its target, a repo "repo@ref", an app its name.
type runPolicyChange struct {
	Cause   string   `json:"cause"` // closed set, see causeOrder
	Field   string   `json:"field"` // RunPolicySpec json name
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	// Detail is the clamp_warnings lines, verbatim (cause limits only).
	Detail  []string   `json:"detail,omitempty"`
	Profile string     `json:"profile,omitempty"`
	At      *time.Time `json:"at,omitempty"` // restart time
}

type storedPolicyNow struct {
	State     string     `json:"state"` // same | changed | updated (older run: edited since) | deleted
	Name      string     `json:"name,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

const (
	policyViewRecorded = "recorded"
	policyViewNotYet   = "not_yet"
	policyViewNever    = "never"
)

// handleGetRunPolicy serves the policy a run got: the envelope dispatch wrote
// as run.policy.resolve (exactly what the proxy enforces), where it started
// from, and why each difference exists. Read gate: owner or admin, foreign runs
// 404 byte-identically (getRunAuthorized). Not on the delegation list.
func (s *Server) handleGetRunPolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorized(w, r, id)
	if !ok {
		return
	}
	ctx := r.Context()
	events, err := s.cfg.Store.QueryAuditEvents(ctx, run.ID, effectivePolicyAuditScan)
	if err != nil {
		writeServerError(w, r, "read run policy", err)
		return
	}
	create := runCreateFacts(events)
	resp := runPolicyResponse{RunID: run.ID, Changes: []runPolicyChange{}, Complete: create.Source != nil}
	resp.Source = s.runPolicySourceOf(ctx, run, create)

	envelope, at, found := latestPolicyResolve(events)
	if !found {
		resp.State = policyViewNotYet
		if run.State.IsTerminal() {
			resp.State = policyViewNever
		}
		resp.StoredPolicyNow = s.storedPolicyNowOf(ctx, run, create, &resp.Source)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.State, resp.RecordedAt = policyViewRecorded, &at

	revives, err := s.runReviveEvents(ctx, run.ID)
	if err != nil {
		writeServerError(w, r, "read run restarts", err)
		return
	}
	ev := collectEvidence(events, revives)
	ev.diskFilled = envelope.DiskMiBFilled
	ev.bounded = create.Source != nil && create.Source.Bounded
	if create.Source != nil {
		ev.clamp = create.ClampWarnings
	} else {
		ev.legacyGitBroker = s.holdsGitHubGrant(ctx, run.ID)
	}

	served := envelope.RunPolicySpec
	for _, h := range ev.restartHosts {
		if !slices.Contains(served.DeniedDomains, h) {
			served.DeniedDomains = append(served.DeniedDomains, h)
		}
	}
	var base *types.RunPolicySpec
	if create.Source != nil {
		base = &create.Source.Spec
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		slog.WarnContext(ctx, "api: run policy view without the site config", "run_id", run.ID, "err", err)
	}
	resp.Changes = explainRunPolicy(base, served, ev, sc, run)
	resp.StoredPolicyNow = s.storedPolicyNowOf(ctx, run, create, &resp.Source)

	secOp := s.isSecurityOperator(ctx)
	out := redactSpecForRead(served, secOp)
	if out.LLMInspection != nil {
		li := *out.LLMInspection
		li.WorkspaceSecretValues = nil // a placeholder count would fail validatePolicySpec's no-raw-values rule
		out.LLMInspection = &li
	}
	if out.AllowedDomains == nil {
		out.AllowedDomains = []string{}
	}
	resp.Redacted = !secOp && !reflect.DeepEqual(specJSON(served), specJSON(redactSpecForUser(served)))
	resp.Spec = &out
	writeJSON(w, http.StatusOK, resp)
}

// createFacts is what the run's success run.create row says about its policy.
type createFacts struct {
	Source        *policySourceRecord `json:"policy_source"`
	PolicyID      *uuid.UUID          `json:"policy_id"`
	InlinePolicy  *bool               `json:"inline_policy"` // absent on lanes that write their own row
	ClampWarnings []string            `json:"clamp_warnings"`
}

// runCreateFacts reads the run's success run.create row. Only success rows: the
// failure family reuses the action name (runModelProviderFacts does the same).
func runCreateFacts(events []types.AuditEvent) createFacts {
	var f createFacts
	for _, ev := range events {
		if canonicalAction(ev.Action) == "run.create" && ev.Outcome == "success" && json.Unmarshal(ev.Data, &f) == nil {
			return f
		}
	}
	return createFacts{}
}

// runPolicySourceOf is where the policy started: the recorded source when the
// run has one, else what an older run's create row still states, with a live
// name lookup.
func (s *Server) runPolicySourceOf(ctx context.Context, run types.AgentRun, c createFacts) runPolicySource {
	out := runPolicySource{Preset: run.Preset, PresetVersion: run.PresetVersion}
	switch {
	case c.Source != nil:
		out.Kind, out.PolicyID, out.Name = c.Source.Kind, c.Source.PolicyID, c.Source.Name
	case c.PolicyID != nil:
		out.Kind, out.PolicyID = policyKindStored, c.PolicyID
		if p, err := s.cfg.Store.GetPolicy(ctx, *c.PolicyID); err == nil {
			out.Name = p.Name
		}
	case c.InlinePolicy != nil && *c.InlinePolicy:
		out.Kind = policyKindInline
	case c.InlinePolicy != nil && run.GovernanceProfileID != nil:
		out.Kind = policyKindProfile
		if ps, err := s.cfg.Store.ListGovernanceProfiles(ctx); err == nil {
			if i := slices.IndexFunc(ps, func(p types.GovernanceProfile) bool { return p.ID == *run.GovernanceProfileID }); i >= 0 {
				out.Name = ps[i].Name
			}
		}
	case c.InlinePolicy != nil:
		out.Kind = policyKindDefault
	default:
		out.Kind = policyKindUnknown
	}
	return out
}

// storedPolicyNowOf says whether the saved policy a run started from still
// reads the way it did. It is computed here because the reader may have lost
// access to the saved policy. With a recorded base the two redacted forms are
// compared; an older run can only say whether the row was updated after launch,
// which a rename alone also does.
func (s *Server) storedPolicyNowOf(ctx context.Context, run types.AgentRun, c createFacts, src *runPolicySource) *storedPolicyNow {
	if src.Kind != policyKindStored || src.PolicyID == nil {
		return nil
	}
	cur, err := s.cfg.Store.GetPolicy(ctx, *src.PolicyID)
	if errors.Is(err, store.ErrNotFound) {
		src.Deleted = true
		return &storedPolicyNow{State: "deleted"}
	}
	if err != nil {
		slog.WarnContext(ctx, "api: run policy view could not read the saved policy", "run_id", run.ID, "err", err)
		return nil
	}
	now := &storedPolicyNow{State: "same", Name: cur.Name, UpdatedAt: &cur.UpdatedAt}
	switch {
	case c.Source != nil:
		if !reflect.DeepEqual(specJSON(redactSpecForUser(auditablePolicy(cur.Spec))), specJSON(c.Source.Spec)) {
			now.State = "changed"
		}
	case cur.UpdatedAt.After(run.CreatedAt):
		now.State = "updated"
	}
	return now
}

// specJSON is a spec in canonical, comparable form: one JSON round trip
// through generic values, so key order (jsonb reorders) and a nil versus an
// empty allowlist cannot make two equal policies differ.
func specJSON(sp types.RunPolicySpec) any {
	if sp.AllowedDomains == nil {
		sp.AllowedDomains = []string{}
	}
	var v any
	b, _ := json.Marshal(sp)
	_ = json.Unmarshal(b, &v)
	return v
}

// runReviveEvents is the run's run.revive rows. A restart can come late in a
// long trail, past the window the launch rows are read from, so it is asked
// for by action.
func (s *Server) runReviveEvents(ctx context.Context, runID uuid.UUID) ([]types.AuditEvent, error) {
	f := store.AuditFilter{ActionPrefix: "run.revive"}
	if p, ok := s.cfg.Store.(store.Pager); ok {
		return p.QueryAuditEventsFilteredPage(ctx, &runID, f, store.Page{Limit: 200})
	}
	all, err := s.cfg.Store.QueryAuditEvents(ctx, runID, 0)
	return f.Keep(all), err
}

// holdsGitHubGrant is rule 12's gate: confineGitBrokerEgress does nothing for a
// run without a github grant, so a broker-managed deny on an older run means
// the connection only if it held one. A read failure says no.
func (s *Server) holdsGitHubGrant(ctx context.Context, runID uuid.UUID) bool {
	grants, err := s.cfg.Store.ListGrantsByRun(ctx, runID)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(grants, func(g types.CredentialGrant) bool { return g.Spec.Kind == types.GrantGitHubToken })
}

// evidenceRows is the payload subset of the launch audit rows that says why
// an entry is in the policy. Fields a row does not have stay zero.
type evidenceRow struct {
	Kind           string   `json:"kind"`
	AddedDomains   []string `json:"added_domains"`
	Hosts          []string `json:"hosts"`
	Host           string   `json:"host"`
	Port           int      `json:"port"`
	Profile        string   `json:"profile"`
	DeniedAdded    []string `json:"denied_added"`
	MaxEphemeralMB int      `json:"max_ephemeral_disk_mib"`
}

// collectEvidence sorts the run's launch rows into the sets explainRunPolicy
// matches entries against. Only success rows count. restartHosts keeps the
// order restart denies were added in.
func collectEvidence(events, revives []types.AuditEvent) auditEvidence {
	ev := auditEvidence{
		workspace: map[string]bool{}, sourceControl: map[string]bool{}, mirror: map[string]bool{}, model: map[string]bool{},
		confine: map[string]bool{}, profileDenied: map[string]bool{}, mirrorHosts: map[string]bool{},
		restart: map[string]time.Time{},
	}
	add := func(set map[string]bool, hosts ...string) {
		for _, h := range hosts {
			set[h] = true
		}
	}
	for _, e := range slices.Concat(events, revives) {
		if e.Outcome != "success" {
			continue
		}
		var row evidenceRow
		if json.Unmarshal(e.Data, &row) != nil {
			continue
		}
		switch canonicalAction(e.Action) {
		case "run.egress.add":
			switch row.Kind {
			case "workspace", "workspace_clone":
				add(ev.workspace, row.AddedDomains...)
			case "site_config", "ssh", "git_pat":
				add(ev.sourceControl, row.AddedDomains...)
			}
		case "run.requirement.allow", "run.requirement.grant", "run.requirement.inject":
			add(ev.workspace, row.AddedDomains...)
		case "run.artifact.redirect":
			if row.Host != "" {
				add(ev.mirror, row.Host, net.JoinHostPort(row.Host, strconv.Itoa(row.Port)))
				add(ev.mirrorHosts, strings.ToLower(row.Host))
			}
		case "run.bedrock.configure":
			add(ev.model, row.Hosts...)
		case "run.egress.confine":
			add(ev.confine, row.Hosts...)
		case "run.ceiling.reassert":
			ev.profile, ev.profileMaxDisk = row.Profile, row.MaxEphemeralMB
			add(ev.profileDenied, row.DeniedAdded...)
		case "run.revive":
			for _, h := range row.DeniedAdded {
				if _, seen := ev.restart[h]; !seen {
					ev.restart[h] = e.Time
					ev.restartHosts = append(ev.restartHosts, h)
				}
			}
		}
	}
	return ev
}
