// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A person's own walls (#2948): one private, narrow-only document (types.PersonalWalls) kept in
// principal_prefs under the person's own principal. It is met into every run they start by one step
// just inside the Wardyn boundary's (personalWallsRunStep), and its denies, desktop blocks and withheld
// credentials are re-asserted at dispatch, at revive and at the mint door by one seal layer
// (personalWallsSealLayer). Nobody else reads or writes it, and it can only narrow.

// personalProvenance is the record of a value the person's own walls decided.
var personalProvenance = policyref.Provenance{Layer: policyref.LayerPersonal}

const (
	// maxWallsDomains caps each site list, so the run step's meet is bounded (≤ 256 × 64 entries).
	maxWallsDomains = 64
	// maxWallsWithheld caps the credentials a person keeps out.
	maxWallsWithheld = 16

	auditWallsSet    = "person.walls.set"
	auditWallsDelete = "person.walls.delete"
)

// mountPersonalWallsRoutes mounts the caller's own walls on the authenticated group: keyed by the
// signed-in person, never a principal from the path or the body, and with no admin twin.
func (s *Server) mountPersonalWallsRoutes(r chi.Router) {
	r.Get("/me/walls", s.handleGetMyWalls)
	r.Put("/me/walls", s.handlePutMyWalls)
	r.Delete("/me/walls", s.handleDeleteMyWalls)
}

// wallsPerson is the person whose walls a request reads, writes and launches under: the signed-in
// human, who may act through an API token, a CLI session or a delegation. The admin token, local
// mode, a service identity, a device and an approval rule are no person and have no walls.
func wallsPerson(ctx context.Context) string {
	if _, _, ok := serviceIdentityFromContext(ctx); ok {
		return ""
	}
	if _, ok := deviceFromContext(ctx); ok {
		return ""
	}
	if _, ok := policyDeciderFrom(ctx); ok {
		return ""
	}
	if localPrincipalFromContext(ctx) != "" {
		return ""
	}
	return oidcHumanFromContext(ctx)
}

// readPersonalWalls is principal's stored document: none when nothing was saved. A document that
// cannot be read is an error, never no walls.
func readPersonalWalls(ctx context.Context, prefs store.PrincipalPrefStore, principal string) (types.PersonalWalls, error) {
	var doc types.PersonalWalls
	raw, err := prefs.GetPrincipalPref(ctx, principal, types.PersonalWallsPrefKey)
	if errors.Is(err, store.ErrNotFound) {
		return doc, nil
	}
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		err = dec.Decode(&doc)
	}
	if err != nil {
		return types.PersonalWalls{}, fmt.Errorf("api: read the person's walls: %w", err)
	}
	return doc, nil
}

// personalWalls is principal's walls: none for no principal, and none on a store that keeps no
// preferences, where none can have been saved.
func (s *Server) personalWalls(ctx context.Context, principal string) (types.PersonalWallsSpec, error) {
	prefs, ok := s.cfg.Store.(store.PrincipalPrefStore)
	if !ok || principal == "" {
		return types.PersonalWallsSpec{}, nil
	}
	doc, err := readPersonalWalls(ctx, prefs, principal)
	return doc.Spec, err
}

// wallsPrefs is the store and the person a /me/walls route acts for: 400 walls_need_person for a
// caller who is no person, 501 walls_unavailable on a store that keeps no preferences.
func (s *Server) wallsPrefs(w http.ResponseWriter, r *http.Request) (store.PrincipalPrefStore, string, bool) {
	person := wallsPerson(r.Context())
	if person == "" {
		writeErrorReason(w, http.StatusBadRequest, reasonWallsNeedPerson,
			"Walls belong to a signed-in person. The admin token, local mode and a service identity have none.")
		return nil, "", false
	}
	prefs, ok := s.cfg.Store.(store.PrincipalPrefStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonWallsUnavailable,
			"This deployment keeps no personal settings, so walls cannot be kept here.")
		return nil, "", false
	}
	return prefs, person, true
}

// handleGetMyWalls answers the caller's own walls: {spec, updated_at}, or {spec: {}} when none are set.
func (s *Server) handleGetMyWalls(w http.ResponseWriter, r *http.Request) {
	prefs, person, ok := s.wallsPrefs(w, r)
	if !ok {
		return
	}
	doc, err := readPersonalWalls(r.Context(), prefs, person)
	if err != nil {
		writeServerError(w, r, "read your walls", err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// handlePutMyWalls replaces the caller's walls with the body's spec, whole. An empty spec is no walls.
// No second human is asked: the walls narrow only the writer's own runs.
func (s *Server) handlePutMyWalls(w http.ResponseWriter, r *http.Request) {
	prefs, person, ok := s.wallsPrefs(w, r)
	if !ok {
		return
	}
	var body types.PersonalWalls
	if !decodeStrict(w, r, &body) {
		return
	}
	if err := validatePersonalWalls(body.Spec); err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonWallsInvalid, "invalid walls: "+err.Error())
		return
	}
	now := s.cfg.Now().UTC()
	doc := types.PersonalWalls{Spec: body.Spec, UpdatedAt: &now}
	if err := prefs.PutPrincipalPref(r.Context(), person, types.PersonalWallsPrefKey, mustJSON(doc)); err != nil {
		writeServerError(w, r, "write your walls", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r), auditWallsSet,
		types.PersonalWallsPrefKey, "success", mustJSON(wallsAuditData(body.Spec))))
	writeJSON(w, http.StatusOK, doc)
}

// handleDeleteMyWalls removes the caller's walls: the stored document becomes {}, as a cleared
// default pool does.
func (s *Server) handleDeleteMyWalls(w http.ResponseWriter, r *http.Request) {
	prefs, person, ok := s.wallsPrefs(w, r)
	if !ok {
		return
	}
	if err := prefs.PutPrincipalPref(r.Context(), person, types.PersonalWallsPrefKey, json.RawMessage(`{}`)); err != nil {
		writeServerError(w, r, "remove your walls", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r), auditWallsDelete,
		types.PersonalWallsPrefKey, "success", mustJSON(map[string]any{"knobs": []string{}})))
	w.WriteHeader(http.StatusNoContent)
}

// wallsAuditData is what a walls write records: the knobs named, the size of each list and the
// kinds kept out, never the lists themselves (they are private; a run's own rows say what bound it).
func wallsAuditData(sp types.PersonalWallsSpec) map[string]any {
	knobs := []string{}
	data := map[string]any{}
	if sp.AllowedDomains != nil {
		knobs = append(knobs, "allowed_domains")
		data["allowed_domains"] = len(*sp.AllowedDomains)
	}
	if sp.DeniedDomains != nil {
		knobs = append(knobs, "denied_domains")
		data["denied_domains"] = len(*sp.DeniedDomains)
	}
	for _, k := range []struct {
		name string
		set  bool
	}{
		{"allow_all_egress", sp.AllowAllEgress != nil}, {"first_use_approval", sp.FirstUseApproval != nil},
		{"min_confinement_class", sp.MinConfinementClass != nil}, {"desktop", sp.Desktop != nil},
	} {
		if k.set {
			knobs = append(knobs, k.name)
		}
	}
	if len(sp.WithheldGrants) > 0 {
		knobs = append(knobs, "withheld_grants")
		kinds := []string{}
		for _, g := range sp.WithheldGrants {
			if !slices.Contains(kinds, string(g.Kind)) {
				kinds = append(kinds, string(g.Kind))
			}
		}
		data["withheld_kinds"] = kinds
	}
	data["knobs"] = knobs
	return data
}

// validatePersonalWalls refuses walls that name anything but a narrowing, naming the first path it
// refuses. A site is a DNS name or a *. wildcard, never an IP address, a port or a URL; a bool may
// only be false and a desktop direction only blocked; the lists are capped.
func validatePersonalWalls(sp types.PersonalWallsSpec) error {
	for _, l := range []struct {
		name string
		list *[]string
	}{{"allowed_domains", sp.AllowedDomains}, {"denied_domains", sp.DeniedDomains}} {
		if l.list == nil {
			continue
		}
		if len(*l.list) > maxWallsDomains {
			return fmt.Errorf("%s: %d entries exceeds the %d-entry limit", l.name, len(*l.list), maxWallsDomains)
		}
		for i, h := range *l.list {
			if !wallsHost(h) {
				return fmt.Errorf("%s[%d]: %q must be a DNS name such as api.example.com, or a wildcard such as *.example.com", l.name, i, h)
			}
		}
	}
	if sp.AllowAllEgress != nil && *sp.AllowAllEgress {
		return errors.New("allow_all_egress: walls only narrow; set it to false, or leave it out")
	}
	if sp.FirstUseApproval != nil && !sp.FirstUseApproval.Valid() {
		return fmt.Errorf("first_use_approval: %q is not always_deny, deny_with_review or wait_for_review", *sp.FirstUseApproval)
	}
	if sp.MinConfinementClass != nil && sp.MinConfinementClass.Rank() == 0 {
		return fmt.Errorf("min_confinement_class: %q is not CC1, CC2 or CC3", *sp.MinConfinementClass)
	}
	if d := sp.Desktop; d != nil {
		for _, dir := range []struct {
			name string
			rule types.TransferRule
		}{{"paste_in", d.PasteIn}, {"copy_out", d.CopyOut}, {"upload", d.Upload}, {"download", d.Download}} {
			if dir.rule != "" && dir.rule != types.TransferBlocked {
				return fmt.Errorf("desktop.%s: walls only block; set it to blocked, or leave it out", dir.name)
			}
		}
	}
	if len(sp.WithheldGrants) > maxWallsWithheld {
		return fmt.Errorf("withheld_grants: %d entries exceeds the %d-entry limit", len(sp.WithheldGrants), maxWallsWithheld)
	}
	for i, g := range sp.WithheldGrants {
		path := fmt.Sprintf("withheld_grants[%d]", i)
		if err := authoredGrantRefusal(path, g); err != nil {
			return err
		}
		if _, ok := withheldGrantKinds[g.Kind]; !ok {
			return fmt.Errorf("%s: unknown grant kind %q", path, g.Kind)
		}
		if !withheldScopeless(g.Scope) && !validWithheldScope(g) {
			return fmt.Errorf("%s: the scope does not decode into a usable pairing", path)
		}
	}
	return nil
}

// wallsHost is a site a person may list: the hosts.add_denied rule (run_contract.go), a DNS name or a
// wildcard with no IP address, port or URL.
func wallsHost(h string) bool {
	return overrideText(h) && net.ParseIP(h) == nil && !strings.ContainsAny(h, ":/ ") && proxy.ValidDomainEntry(h) == nil
}

// wallsSourceRecord is policy_source.personal_walls on the run.create row: the knobs the person's
// walls narrowed at create, the allowlist entries and grants (kind:host, grantKey) they took out of
// the run, and the denies they added. Read back by GET /runs/{id}/policy.
type wallsSourceRecord struct {
	Narrowed []string            `json:"narrowed"`
	Removed  map[string][]string `json:"removed,omitempty"`
	Added    map[string][]string `json:"added,omitempty"`
}

// newWallsSource is the record of the walls' step from before to after, nil when it changed nothing.
func newWallsSource(before, after types.RunPolicySpec) *wallsSourceRecord {
	changes := boundaryChanges(before, after, types.GovernanceLimits{}, types.GovernanceLimits{})
	if len(changes) == 0 {
		return nil
	}
	rec := &wallsSourceRecord{}
	for _, c := range changes {
		rec.Narrowed = append(rec.Narrowed, c.Knob)
	}
	put := func(m *map[string][]string, field string, entries []string) {
		if len(entries) == 0 {
			return
		}
		if *m == nil {
			*m = map[string][]string{}
		}
		(*m)[field] = entries
	}
	b, a := canonicalKnobs(before), canonicalKnobs(after)
	put(&rec.Removed, "allowed_domains", setDiff(b.AllowedDomains, a.AllowedDomains))
	put(&rec.Removed, "eligible_grants", setDiff(wallsGrantKeys(before.EligibleGrants), wallsGrantKeys(after.EligibleGrants)))
	put(&rec.Added, "denied_domains", setDiff(a.DeniedDomains, b.DeniedDomains))
	return rec
}

func wallsGrantKeys(gs []types.GrantSpec) []string {
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, grantKey(g))
	}
	return out
}

// personalWallsRunStep narrows spec by the walls of the person launching the run: composer.MeetWalls,
// then the grants they keep out are removed. It is the step just inside the Wardyn boundary's on every
// arm (boundaryThenNoRepositories), so a super admin's own run is bound by their walls too. Each knob it
// changes is recorded as the person's, and named in a warning and on the run's policy source. Walls
// that cannot be read refuse the run: it is never resolved without them.
func (s *Server) personalWallsRunStep(ctx context.Context, spec *types.RunPolicySpec, source *policySourceRecord) ([]string, *runRefusal) {
	walls, err := s.personalWalls(ctx, wallsPerson(ctx))
	if err != nil {
		return nil, runServerError("read your walls", err)
	}
	if walls.Empty() {
		return nil, nil
	}
	before := spec.Clone()
	source.knobs.step(personalProvenance, spec, func() {
		var met types.RunPolicySpec
		if met, err = composer.MeetWalls(*spec, walls.Overlay()); err != nil {
			return
		}
		kept := met.EligibleGrants[:0:0]
		for _, g := range met.EligibleGrants {
			if !grantWithheldBy(g, walls.WithheldGrants) {
				kept = append(kept, g)
			}
		}
		met.EligibleGrants = kept
		*spec = met
	})
	if err != nil {
		return nil, runServerError("meet your walls", err)
	}
	rec := newWallsSource(before, *spec)
	if rec == nil {
		return nil, nil
	}
	source.PersonalWalls = rec
	warns := make([]string, 0, len(rec.Narrowed))
	for _, knob := range rec.Narrowed {
		warns = append(warns, "your walls narrowed "+knob)
	}
	return warns, nil
}

// personalWallsSealLayer is the run owner's walls as a seal layer: their denies, the desktop
// directions they block and the credentials they keep out. Never their allowlist, so the hosts the
// run's own setup adds (model, repositories, components) stay reachable. A run with no owner, an owner
// with no walls, or walls that name none of the three, has none. Walls that cannot be read seal the
// run to no egress and no credential, as an unread restriction does.
func (s *Server) personalWallsSealLayer(ctx context.Context, run types.AgentRun) []sealLayer {
	if run.CreatedBy == "" {
		return nil
	}
	walls, err := s.personalWalls(ctx, run.CreatedBy)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: read the run owner's walls at the seal; the run is sealed to no egress",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		off := false
		return []sealLayer{{rec: personalProvenance, egress: types.CeilingOverlay{AllowedDomains: &[]string{}, AllowAllEgress: &off},
			grants: &[]types.GrantSpec{}}}
	}
	l := sealLayer{rec: personalProvenance, desktop: walls.Overlay().Desktop, withheldGrants: walls.WithheldGrants}
	if d := walls.DeniedDomains; d != nil && len(*d) > 0 {
		deny := slices.Clone(*d)
		l.egress.DeniedDomains = &deny
	}
	if l.egress.DeniedDomains == nil && l.desktop == nil && len(l.withheldGrants) == 0 {
		return nil
	}
	return []sealLayer{l}
}

// wallsAdmitApproval is the run owner's walls' bound on an egress approval, beside the boundary's and
// the restrictions': a host their walls deny is not admitted, whoever decides. A host outside their
// allowlist follows their own choice for a site that is not listed, so it is not refused here. Walls
// that cannot be read refuse the approve (500). ok=false: the answer is written and the approval stays
// pending. It returns the run, read here when the gates before it had not.
func (s *Server) wallsAdmitApproval(w http.ResponseWriter, r *http.Request, ap types.ApprovalRequest, run types.AgentRun, haveRun bool) (types.AgentRun, bool, bool) {
	if _, ok := s.cfg.Store.(store.PrincipalPrefStore); !ok {
		return run, haveRun, true
	}
	if !haveRun {
		var err error
		if run, err = s.cfg.Store.GetRun(r.Context(), ap.RunID); err != nil {
			writeServerError(w, r, "get run", err)
			return run, false, false
		}
		haveRun = true
	}
	walls, err := s.personalWalls(r.Context(), run.CreatedBy)
	if err != nil {
		writeServerError(w, r, "read the run owner's walls", err)
		return run, haveRun, false
	}
	host := approvalHost(ap)
	if d := walls.DeniedDomains; d == nil || !ceilingDenies(*d, host) {
		return run, haveRun, true
	}
	msg := run.CreatedBy + "'s walls block " + host + " for this run."
	if principalFromRequest(r) == run.CreatedBy {
		msg = "Your walls block " + host + ". Change them in Settings."
	}
	ref := &policyref.Ref{Source: policyref.LayerPersonal, Owner: run.CreatedBy}
	writeErrorReasonPolicy(w, http.StatusForbidden, reasonPersonalWallsHost, msg, ref, personalProvenance)
	return run, haveRun, false
}

// projectWallsBlock sets WallsBlock on every PENDING egress_domain row whose host the run owner's walls
// deny, the verdict wallsAdmitApproval gives on an approve. The run and the walls are read once each per run;
// a run or walls that cannot be read leave the row unmarked, and the approve still refuses on its own.
func (s *Server) projectWallsBlock(ctx context.Context, rows []types.ApprovalRequest) {
	if _, ok := s.cfg.Store.(store.PrincipalPrefStore); !ok {
		return
	}
	blocks := map[uuid.UUID]*types.PersonalWallsSpec{}
	for i := range rows {
		ap := rows[i]
		if ap.State != types.ApprovalPending || ap.Kind != types.ApprovalEgressDomain {
			continue
		}
		walls, seen := blocks[ap.RunID]
		if !seen {
			if run, err := s.cfg.Store.GetRun(ctx, ap.RunID); err == nil {
				if spec, err := s.personalWalls(ctx, run.CreatedBy); err == nil {
					walls = &spec
				}
			}
			blocks[ap.RunID] = walls
		}
		if walls != nil && walls.DeniedDomains != nil && ceilingDenies(*walls.DeniedDomains, approvalHost(ap)) {
			rows[i].WallsBlock = true
		}
	}
}
