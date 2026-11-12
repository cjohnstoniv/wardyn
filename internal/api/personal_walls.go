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

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
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
	// maxWallsDenyPaths caps the push deny list a person adds.
	maxWallsDenyPaths = 64
	// maxWallsCapabilities caps each capability list; the closed sets are far smaller, so this only
	// bounds a list that repeats itself.
	maxWallsCapabilities = 64

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
	for _, l := range []struct {
		name string
		n    int
		set  bool
	}{
		{"azure_devops_capabilities", wallsLen(sp.AzureDevOpsCapabilities), sp.AzureDevOpsCapabilities != nil},
		{"github_capabilities", wallsLen(sp.GitHubCapabilities), sp.GitHubCapabilities != nil},
		{"tool_rules", wallsLen(sp.ToolRules), sp.ToolRules != nil && len(*sp.ToolRules) > 0},
		{"push_rules.deny_paths", wallsLen(sp.AddedDenyPaths()), sp.AddedDenyPaths() != nil},
	} {
		if l.set {
			knobs = append(knobs, l.name)
			data[l.name] = l.n
		}
	}
	for _, k := range []struct {
		name string
		set  bool
	}{
		{"allow_all_egress", sp.AllowAllEgress != nil}, {"first_use_approval", sp.FirstUseApproval != nil},
		{"push_rules.deny_all", sp.DeniesAllPushes()},
		{"min_confinement_class", sp.MinConfinementClass != nil}, {"desktop", sp.Desktop != nil},
		{"recording", sp.Recording != nil}, {"auto_stop_after_sec", sp.AutoStopAfterSec != nil},
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
	if err := validateWallsNarrowing(sp); err != nil {
		return err
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

// validateWallsNarrowing is validatePersonalWalls for the capability lists, tool rules, push deny paths,
// recording floor and auto-stop cap, each held to what the composer's meet can only narrow with. It
// reuses the policy writer's own checks (capability catalogues, validateToolRules, validatePushRules).
func validateWallsNarrowing(sp types.PersonalWallsSpec) error {
	if l := sp.AzureDevOpsCapabilities; l != nil {
		if len(*l) == 0 || len(*l) > maxWallsCapabilities {
			return fmt.Errorf("azure_devops_capabilities: name between 1 and %d capabilities, or leave it out", maxWallsCapabilities)
		}
		if err := validateADOCapabilities(*l); err != nil {
			return err
		}
	}
	if l := sp.GitHubCapabilities; l != nil {
		if len(*l) == 0 || len(*l) > maxWallsCapabilities {
			return fmt.Errorf("github_capabilities: name between 1 and %d capabilities, or leave it out", maxWallsCapabilities)
		}
		for i, c := range *l {
			if !c.Grantable() {
				return fmt.Errorf("github_capabilities[%d]: %q is not a grantable GitHub capability — want one of: %s", i, string(c), ghscope.GrantableCapabilityList())
			}
		}
	}
	if sp.ToolRules != nil {
		if err := validateToolRules(*sp.ToolRules); err != nil {
			return err
		}
		for i, r := range *sp.ToolRules {
			if r.Effect == types.ToolAllow {
				return fmt.Errorf("tool_rules[%d]: walls only hold or deny a tool; %q cannot be allowed", i, r.Tool)
			}
		}
	}
	if sp.PushRules != nil && sp.PushRules.DenyAll != nil && !*sp.PushRules.DenyAll {
		return errors.New("push_rules.deny_all: walls only narrow; set it to true, or leave it out")
	}
	if sp.PushRules != nil && sp.PushRules.DenyPaths != nil {
		if len(*sp.PushRules.DenyPaths) > maxWallsDenyPaths {
			return fmt.Errorf("push_rules.deny_paths: %d entries exceeds the %d-entry limit", len(*sp.PushRules.DenyPaths), maxWallsDenyPaths)
		}
		if err := validatePushRulePaths("deny_paths", *sp.PushRules.DenyPaths); err != nil {
			return err
		}
	}
	if r := sp.Recording; r != nil {
		switch {
		case r.Rank() == 0:
			return fmt.Errorf("recording: %q is not terminal or screen_actions", string(*r))
		case *r == types.RecordingOff:
			return errors.New("recording: off is no floor, so it narrows nothing; set terminal or screen_actions, or leave it out")
		}
	}
	if v := sp.AutoStopAfterSec; v != nil && *v <= 0 {
		return fmt.Errorf("auto_stop_after_sec: %d is not a cap; use a number of seconds above zero (zero or less means never stop)", *v)
	}
	return nil
}

// wallsLen is the length of a typed-presence list, 0 when it is absent.
func wallsLen[T any](l *[]T) int {
	if l == nil {
		return 0
	}
	return len(*l)
}

// wallsHost is a site a person may list: the hosts.add_denied rule (run_contract.go), a DNS name or a
// wildcard with no IP address, port or URL.
func wallsHost(h string) bool {
	return overrideText(h) && net.ParseIP(h) == nil && !strings.ContainsAny(h, ":/ ") && proxy.ValidDomainEntry(h) == nil
}

// wallsSourceRecord is policy_source.personal_walls on the run.create row: the knobs the person's
// walls narrowed at create, the allowlist entries and grants (kind:host, grantKey) they took out of
// the run, and the denies and blocked desktop directions they added. Read back by GET /runs/{id}/policy.
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
	put(&rec.Added, fieldDesktop, setDiff(desktopBlocked(after), desktopBlocked(before)))
	// Every push refused is a knob inside push_rules, so Narrowed names push_rules alone; the console reads this entry to say
	// which layer refuses every push.
	if after.PushRules != nil && after.PushRules.DenyAll && (before.PushRules == nil || !before.PushRules.DenyAll) {
		put(&rec.Added, fieldPushRules, []string{pushRulesDenyAll})
	}
	return rec
}

func wallsGrantKeys(gs []types.GrantSpec) []string {
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, grantKey(g))
	}
	return out
}

// personalWallsRunStep narrows spec by the walls of the person launching the run (wallsRunStep). It is
// the step just inside the Wardyn boundary's on every arm (boundaryThenNoRepositories), so a super
// admin's own run is bound by their walls too. Each knob it changes is recorded as the person's, and
// named in a warning and on the run's policy source. Walls that cannot be read refuse the run: it is
// never resolved without them.
func (s *Server) personalWallsRunStep(ctx context.Context, spec *types.RunPolicySpec, source *policySourceRecord) ([]string, *runRefusal) {
	walls, err := s.personalWalls(ctx, wallsPerson(ctx))
	if err != nil {
		return nil, runServerError("read your walls", err)
	}
	rec, err := s.wallsRunStep(spec, source, walls, personalProvenance)
	if err != nil {
		return nil, wallsMeetRefusal(err, "your walls", "Your walls", "Change your walls with `wardyn walls`, or in Settings.", true)
	}
	source.PersonalWalls = rec
	return wallsWarnings("your walls", rec), nil
}

// wallsRunStep narrows spec by one walls document, a person's own or a wall preset's: composer.MeetWalls
// over walls.Overlay(), so every knob the overlay carries is met and none is copied by hand, then the
// grants the walls keep out are removed, then wallsBeyondMeet. Each knob it changes is recorded as rec's.
// It returns what it changed, nil when nothing; an error is the meet's.
func (s *Server) wallsRunStep(spec *types.RunPolicySpec, source *policySourceRecord, walls types.PersonalWallsSpec, rec policyref.Provenance) (*wallsSourceRecord, error) {
	if walls.Empty() {
		return nil, nil
	}
	before := spec.Clone()
	var err error
	source.knobs.step(rec, spec, func() {
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
		s.wallsBeyondMeet(&met, *spec, walls)
		*spec = met
	})
	if err != nil {
		return nil, err
	}
	return newWallsSource(before, *spec), nil
}

// wallsMeetRefusal answers a walls meet that failed: 403 when the walls and the policy have nothing in
// common for a knob, named as who (mid-sentence) and Who (at its start), with the remedy and, when
// detail, the values that did not meet; else a server error.
func wallsMeetRefusal(err error, who, whoStart, remedy string, detail bool) *runRefusal {
	var unmet *composer.OverlayError
	if errors.As(err, &unmet) && unmet.Reason == composer.ReasonOverlayUnsatisfiable {
		values := ""
		if detail {
			values = ": " + unmet.Detail
		}
		return runError(http.StatusForbidden, reasonGovernanceOverlayUnsatisfiable,
			whoStart+" and this run's policy leave "+unmet.Field+" nothing in common"+values+". "+remedy)
	}
	return runServerError("meet "+who, err)
}

// wallsWarnings names each knob rec narrowed, as who's.
func wallsWarnings(who string, rec *wallsSourceRecord) []string {
	if rec == nil {
		return nil
	}
	warns := make([]string, 0, len(rec.Narrowed))
	for _, knob := range rec.Narrowed {
		warns = append(warns, who+" narrowed "+knob)
	}
	return warns
}

// launchWallPreset is the frozen copy of the wall preset a launch names, nil when it names none. A
// built-in is open to every caller; a stored preset is the caller's to apply when the templates'
// authority lets them read it, judged on the one owner subject (wallPresetCaller). One that does not
// exist, that the caller cannot read, or that names a stored preset on a store that keeps none is
// refused 422 wall_preset_unknown, the same for each: a launch is no existence oracle, and it is never
// run without the preset it named.
func (s *Server) launchWallPreset(ctx context.Context, r *http.Request, id string) (*types.WallPresetOnRun, *runRefusal) {
	if id == "" {
		return nil, nil
	}
	p, ok := types.BuiltinWallPreset(id)
	if !ok {
		unknown := runError(http.StatusUnprocessableEntity, reasonWallPresetUnknown,
			fmt.Sprintf("wall preset %q does not exist or is not open to you", id))
		ps, has := s.cfg.Store.(store.WallPresetStore)
		if !has {
			return nil, unknown
		}
		c, err := s.wallPresetCaller(ctx, r, ps)
		if err != nil {
			return nil, runServerError("read template grants", err)
		}
		var d types.TemplateDenial
		p, d, err = findWallPreset(ctx, ps, c.actor, id)
		switch {
		case errors.Is(err, store.ErrNotFound), err == nil && d != types.TemplateAllowed:
			return nil, unknown
		case err != nil:
			return nil, runServerError("read the wall preset", err)
		}
	}
	return &types.WallPresetOnRun{ID: p.ID, Name: p.Name, Scope: p.Scope, GroupID: p.GroupID, Version: p.Version, Spec: p.Spec}, nil
}

// wallPresetSourceRecord is policy_source.wall_preset on the run.create row: the preset the run was
// launched with and, as for a person's walls, what it narrowed, removed and added.
type wallPresetSourceRecord struct {
	ID      string                `json:"id"`
	Name    string                `json:"name"`
	Scope   types.WallPresetScope `json:"scope"`
	GroupID string                `json:"group_id,omitempty"`
	Version int                   `json:"version"`
	*WallsSourceRecord
}

// WallsSourceRecord is wallsSourceRecord under an exported name, for wallPresetSourceRecord's embed: the
// record's fields stay at wall_preset's top level, and encoding/json can only allocate an embedded pointer
// when it decodes through an exported field (an unexported one fails the whole run.create row's decode).
type WallsSourceRecord = wallsSourceRecord

// wallPresetProvenance is the record of a value a run's wall preset decided. A personal preset's name
// is its owner's to see, so its record names none.
func wallPresetProvenance(p *types.WallPresetOnRun) policyref.Provenance {
	rec := policyref.Provenance{Layer: policyref.LayerWallPreset}
	if p.Scope != types.WallPresetScopePerson {
		rec.Name = p.Name
	}
	return rec
}

// wallPresetLabel names the run's preset in a sentence read by the run's owner (owner) or someone
// else, who is told only that a personal preset applied.
func wallPresetLabel(p *types.WallPresetOnRun, owner bool) string {
	if p.Scope == types.WallPresetScopePerson && !owner {
		return "a personal wall preset"
	}
	return "the " + p.Name + " wall preset"
}

// wallPresetKept names the run's preset in text kept on the run or in its audit (a warning, a refusal
// that becomes a queued run's end, a dispatch refusal), which more than its owner read: a personal
// preset is never named there, nor are its values quoted (open is false). who starts mid-sentence,
// whoStart a sentence.
func wallPresetKept(p *types.WallPresetOnRun) (who, whoStart string, open bool) {
	who = wallPresetLabel(p, false)
	return who, strings.ToUpper(who[:1]) + who[1:], p.Scope != types.WallPresetScopePerson
}

// wallPresetRunStep narrows spec by the wall preset the launch chose, after the person's own walls and
// by the same step (wallsRunStep), so it only narrows and every knob a preset can name is met. The
// frozen copy is validated again first: a preset a later, stricter rule refuses stops the launch (422
// wall_preset_invalid), never launches without it. Open names nothing and is only recorded.
func (s *Server) wallPresetRunStep(spec *types.RunPolicySpec, source *policySourceRecord, p *types.WallPresetOnRun) ([]string, *runRefusal) {
	if p == nil {
		return nil, nil
	}
	// The warnings are kept on the run and a refusal at a queued start becomes its end, so both name
	// the preset as wallPresetKept does.
	label, labelStart, open := wallPresetKept(p)
	if err := validatePersonalWalls(p.Spec); err != nil {
		why := ""
		if open {
			why = " (" + err.Error() + ")"
		}
		return nil, wallPresetInvalid(label + " no longer passes the rules for walls" + why + "; edit the preset, or pick another")
	}
	rec, err := s.wallsRunStep(spec, source, p.Spec, wallPresetProvenance(p))
	if err != nil {
		return nil, wallsMeetRefusal(err, label, labelStart, "Pick another preset, or launch with Open.", open)
	}
	source.WallPreset = &wallPresetSourceRecord{ID: p.ID, Name: p.Name, Scope: p.Scope, GroupID: p.GroupID, Version: p.Version, WallsSourceRecord: rec}
	source.wallPreset = p
	return wallsWarnings(label, rec), nil
}

// runWallPresetView is the run read's wall_preset: never the spec, and for a personal preset read by
// anyone but the run's owner, its scope alone, with the versions when a change ended the queued run.
func runWallPresetView(r *http.Request, run types.AgentRun) *client.RunWallPreset {
	p := run.WallPreset
	if p == nil {
		return nil
	}
	if p.Scope == types.WallPresetScopePerson && principalFromRequest(r) != run.CreatedBy {
		v := &client.RunWallPreset{Scope: string(p.Scope), Changed: p.Changed}
		if p.Changed != nil {
			v.Version = p.Version
		}
		return v
	}
	return &client.RunWallPreset{ID: p.ID, Name: p.Name, Scope: string(p.Scope), GroupID: p.GroupID, Version: p.Version, Changed: p.Changed}
}

// runWallPresetViewLabelled is runWallPresetView with a group preset's group label added, read the way the templates' and
// the wall preset list's group labels are (TemplateGroupLabels). A group with no label, or a store without labels, adds none.
func (s *Server) runWallPresetViewLabelled(r *http.Request, run types.AgentRun) *client.RunWallPreset {
	v := runWallPresetView(r, run)
	if v == nil || v.Scope != string(types.WallPresetScopeGroup) || v.GroupID == "" {
		return v
	}
	ts, ok := s.cfg.Store.(store.TemplateStore)
	if !ok {
		return v
	}
	labels, err := ts.TemplateGroupLabels(r.Context(), []string{v.GroupID})
	if err != nil {
		slog.WarnContext(r.Context(), "api: read a wall preset's group label for the run read", slog.Any("err", err))
		return v
	}
	v.GroupLabel = labels[v.GroupID]
	return v
}

// unstoppablePushWarning is the run warning for a run whose effective capability list leaves out
// code_write on an install whose git token broker is off: the token goes into the sandbox, where
// nothing holds it to the list, so its pushes cannot be stopped.
const unstoppablePushWarning = "Pushes can't be stopped on this install: this run's capabilities leave out code_write, " +
	"but the git token broker is off, so a git token in the sandbox can still push"

// wallsCapabilityRefusal is why a run's provider capabilities are not narrowed to its owner's walls:
// the walls could not be read, or they share no capability with what the run would hold.
type wallsCapabilityRefusal struct {
	status         int
	reason, detail string
}

// wallsCapabilityMeet is the run owner's capability wall, then the run's wall preset's, over the
// capabilities a provider would give the run, where the run's policy left them to the provider's
// default. The meet runs where that default is known: dispatch for Azure DevOps (wallsADOCapabilities),
// create for the GitHub CLI (wallsGitHubCLICapabilities). It keeps the capabilities in every list, in
// caps' order, so it never adds one the default does not give. kept is nil when neither names a list
// for field. Walls that cannot be read, or a wall that leaves nothing of caps, refuse the run.
func wallsCapabilityMeet[C ~string](ctx context.Context, s *Server, run types.AgentRun, field string, caps []C,
	wallOf func(types.PersonalWallsSpec) *[]C,
) (kept []C, refused *wallsCapabilityRefusal) {
	walls, err := s.personalWalls(ctx, run.CreatedBy)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: read the run owner's walls for "+field+"; the run is refused", slog.Any("err", err))
		return nil, &wallsCapabilityRefusal{status: http.StatusServiceUnavailable, reason: reasonGrantWithheldUnread,
			detail: "Your walls could not be read, so this run's " + field + " cannot be narrowed to them. Walls that cannot be read refuse the run; try again."}
	}
	type capWall struct {
		list                  *[]C
		whoStart, who, remedy string
		open                  bool // the list may be quoted (wallPresetKept)
	}
	lists := []capWall{{wallOf(walls), "Your walls", "your walls", "Change your walls with `wardyn walls`, or in Settings.", true}}
	if p := run.WallPreset; p != nil {
		// The refusal is kept on the run and in its audit, so the preset is named as wallPresetKept does.
		who, whoStart, open := wallPresetKept(p)
		lists = append(lists, capWall{wallOf(p.Spec), whoStart, who, "Pick another preset, or launch with Open.", open})
	}
	named := false
	for _, l := range lists {
		if l.list == nil {
			continue
		}
		from := caps
		if named {
			from = kept
		}
		named, kept = true, nil
		for _, c := range from {
			if slices.Contains(*l.list, c) {
				kept = append(kept, c)
			}
		}
		if len(kept) == 0 {
			list := ""
			if l.open {
				list = " (" + wallsCapList(*l.list) + ")"
			}
			return nil, &wallsCapabilityRefusal{status: http.StatusForbidden, reason: reasonGovernanceOverlayUnsatisfiable,
				detail: l.whoStart + " and this run's provider default leave " + field + " nothing in common: no capability is in both the default (" +
					wallsCapList(from) + ") and " + l.who + list + ". " + l.remedy}
		}
	}
	return kept, nil
}

func wallsCapList[C ~string](caps []C) string {
	names := make([]string, len(caps))
	for i, c := range caps {
		names[i] = string(c)
	}
	return strings.Join(names, ", ")
}

// wallsADOCapabilities narrows a run on the provider row's default profile (the policy named no
// azure_devops_capabilities) to the run owner's walls, at dispatch where that profile is known. A
// policy that names a list met the walls at create (personalWallsRunStep).
func (s *Server) wallsADOCapabilities(ctx context.Context, run types.AgentRun, caps []adoscope.Capability) ([]adoscope.Capability, *wallsCapabilityRefusal) {
	return wallsCapabilityMeet(ctx, s, run, "azure_devops_capabilities", caps,
		func(w types.PersonalWallsSpec) *[]adoscope.Capability {
			if w.AzureDevOpsCapabilities == nil {
				return nil
			}
			expanded := adoscope.ExpandPR(*w.AzureDevOpsCapabilities)
			return &expanded
		})
}

// wallsGitHubCLICapabilities narrows the GitHub CLI's capabilities, the attached rows' default after
// the run's own github_capabilities (persistGitHubCLIGrants), to the run owner's walls. The walls
// meet them on every run.
func (s *Server) wallsGitHubCLICapabilities(ctx context.Context, run types.AgentRun, caps []ghscope.Capability) ([]ghscope.Capability, *wallsCapabilityRefusal) {
	return wallsCapabilityMeet(ctx, s, run, "github_capabilities", caps,
		func(w types.PersonalWallsSpec) *[]ghscope.Capability {
			if w.GitHubCapabilities == nil {
				return nil
			}
			expanded := ghscope.ExpandPR(*w.GitHubCapabilities)
			return &expanded
		})
}

// wallsBeyondMeet applies to met what the meet alone would place wrongly; before is the policy the meet
// started from.
//   - tool_rules: the document's rules are ADDED together by narrowToolRules, against the rules the run
//     had before it, so their order in the document does not matter (a hold or deny only, and only
//     where it is stricter than the tool's effect today). The meet would spell out the "*" default
//     as hold, which tightens every tool the person did not name on a run whose lane lets them run.
//   - recording: a policy that names no level reads as the organisation's, so a floor met into it would
//     replace that default with the lower floor. It is left naming none, and the floor is applied at
//     dispatch (wallsRecordingFloor), after the organisation's default is known. A policy that names a
//     level keeps the meet's: the higher of the two.
func (s *Server) wallsBeyondMeet(met *types.RunPolicySpec, before types.RunPolicySpec, walls types.PersonalWallsSpec) {
	if walls.ToolRules != nil {
		met.ToolRules = narrowToolRules(met.ToolRules, *walls.ToolRules)
	}
	if walls.Recording != nil && before.Recording == "" {
		met.Recording = ""
	}
}

// wallsRecordingFloor is the recording level the run owner's walls and the run's wall preset ask for,
// the higher of the two, read at dispatch; empty when neither names one. An error means the walls cannot
// be read: the caller refuses the run, as the create does.
func (s *Server) wallsRecordingFloor(ctx context.Context, run types.AgentRun) (types.RecordingLevel, error) {
	walls, err := s.personalWalls(ctx, run.CreatedBy)
	if err != nil {
		return "", err
	}
	var floor types.RecordingLevel
	for _, r := range []*types.RecordingLevel{walls.Recording, wallPresetRecording(run.WallPreset)} {
		if r != nil && r.Rank() > floor.Rank() {
			floor = *r
		}
	}
	return floor, nil
}

func wallPresetRecording(p *types.WallPresetOnRun) *types.RecordingLevel {
	if p == nil {
		return nil
	}
	return p.Spec.Recording
}

// personalWallsSealLayer is the run owner's walls as a seal layer: their denies, the desktop
// directions they block, the credentials they keep out and their refusal of every push. Never their
// allowlist, so the hosts the run's own setup adds (model, repositories, components) stay reachable. A
// run with no owner, an owner with no walls, or walls that name none of these, has none. Walls that
// cannot be read seal the run to no egress and no credential, as an unread restriction does.
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
	return wallsSealLayer(personalProvenance, walls)
}

// wallPresetSealLayer is the run's wall preset as a seal layer, built as the owner's walls' is
// (wallsSealLayer) from the frozen copy on the run: no store read, so it cannot fail open or seal the
// run to nothing, and a later edit or delete of the preset changes nothing.
//
// The No network preset adds a layer that empties the allowlist and turns open egress off, so the seal
// removes every host a step after resolve added, at dispatch and at revive alike.
func wallPresetSealLayer(run types.AgentRun) []sealLayer {
	if run.WallPreset == nil {
		return nil
	}
	layers := wallsSealLayer(wallPresetProvenance(run.WallPreset), run.WallPreset.Spec)
	if noNetworkPreset(run.WallPreset) {
		off := false
		layers = append(layers, sealLayer{rec: wallPresetProvenance(run.WallPreset),
			egress: types.CeilingOverlay{AllowedDomains: &[]string{}, AllowAllEgress: &off}})
	}
	return layers
}

// wallsSealLayer is one walls document's seal layer, none when it names nothing the seal re-asserts.
func wallsSealLayer(rec policyref.Provenance, walls types.PersonalWallsSpec) []sealLayer {
	l := sealLayer{rec: rec, desktop: walls.Overlay().Desktop, withheldGrants: walls.WithheldGrants, denyAllPushes: walls.DeniesAllPushes()}
	if d := walls.DeniedDomains; d != nil && len(*d) > 0 {
		deny := slices.Clone(*d)
		l.egress.DeniedDomains = &deny
	}
	if l.egress.DeniedDomains == nil && l.desktop == nil && len(l.withheldGrants) == 0 && !l.denyAllPushes {
		return nil
	}
	return []sealLayer{l}
}

// wallsAdmitApproval is the run owner's walls' bound on an egress approval, and the run's wall
// preset's, beside the boundary's and the restrictions': a host either denies is not admitted, whoever
// decides. A host outside their allowlist follows their own choice for a site that is not listed, so it
// is not refused here. Walls that cannot be read refuse the approve (500). ok=false: the answer is
// written and the approval stays pending. It returns the run, read here when the gates before it had
// not. A run launched with the No network preset is refused every approval, whatever the store keeps.
// Otherwise a store that keeps neither walls nor stored presets has no host to deny: a built-in denies
// none. A run that cannot be read, or a server with no run store, refuses the approve (503
// run_unreadable): without the run its frozen preset cannot be checked, so the approval stays pending.
func (s *Server) wallsAdmitApproval(w http.ResponseWriter, r *http.Request, ap types.ApprovalRequest, run types.AgentRun, haveRun bool) (types.AgentRun, bool, bool) {
	if !haveRun {
		err := errors.New("no run store configured")
		if s.cfg.Store != nil {
			run, err = s.cfg.Store.GetRun(r.Context(), ap.RunID)
		}
		if err != nil {
			writeErrorReason(w, http.StatusServiceUnavailable, reasonRunUnreadable,
				"This approval's run could not be read to check its network preset, so nothing was decided. Try again.")
			return run, false, false
		}
		haveRun = true
	}
	host := approvalHost(ap)
	owner := principalFromRequest(r) == run.CreatedBy
	if p := run.WallPreset; noNetworkPreset(p) {
		msg := "This run was launched with " + wallPresetLabel(p, owner) + ", which refuses every host, " + host + " included. Launch a new run without it."
		prov := wallPresetProvenance(p)
		writeErrorReasonPolicy(w, http.StatusForbidden, reasonWallPresetHost, msg, policyref.WallPreset(prov.Name, run.CreatedBy), prov)
		return run, haveRun, false
	}
	if !s.wallsDenyHosts() {
		return run, haveRun, true
	}
	walls, err := s.personalWalls(r.Context(), run.CreatedBy)
	if err != nil {
		writeServerError(w, r, "read the run owner's walls", err)
		return run, haveRun, false
	}
	if d := walls.DeniedDomains; d != nil && ceilingDenies(*d, host) {
		msg := run.CreatedBy + "'s walls block " + host + " for this run."
		if owner {
			msg = "Your walls block " + host + ". Change them in Settings."
		}
		writeErrorReasonPolicy(w, http.StatusForbidden, reasonPersonalWallsHost, msg, policyref.Personal(run.CreatedBy), personalProvenance)
		return run, haveRun, false
	}
	if p := run.WallPreset; p != nil && p.Spec.DeniedDomains != nil && ceilingDenies(*p.Spec.DeniedDomains, host) {
		msg := "This run was launched with " + wallPresetLabel(p, owner) + ", which blocks " + host + ". Launch a new run without it."
		prov := wallPresetProvenance(p)
		writeErrorReasonPolicy(w, http.StatusForbidden, reasonWallPresetHost, msg, policyref.WallPreset(prov.Name, run.CreatedBy), prov)
		return run, haveRun, false
	}
	return run, haveRun, true
}

// wallsDenyHosts reports whether the store keeps anything that can deny a run a host on an approval:
// a person's walls, or a stored wall preset.
func (s *Server) wallsDenyHosts() bool {
	_, prefs := s.cfg.Store.(store.PrincipalPrefStore)
	_, presets := s.cfg.Store.(store.WallPresetStore)
	return prefs || presets
}

// projectWallsBlock sets WallsBlock on every PENDING egress_domain row whose host the run owner's walls
// or the run's wall preset deny, the verdict wallsAdmitApproval gives on an approve. The run and the
// walls are read once each per run; a run or walls that cannot be read leave the row unmarked, and the
// approve still refuses on its own.
func (s *Server) projectWallsBlock(ctx context.Context, rows []types.ApprovalRequest) {
	if !s.wallsDenyHosts() {
		return
	}
	denies := map[uuid.UUID][][]string{}
	for i := range rows {
		ap := rows[i]
		if ap.State != types.ApprovalPending || ap.Kind != types.ApprovalEgressDomain {
			continue
		}
		lists, seen := denies[ap.RunID]
		if !seen {
			if run, err := s.cfg.Store.GetRun(ctx, ap.RunID); err == nil {
				if spec, err := s.personalWalls(ctx, run.CreatedBy); err == nil && spec.DeniedDomains != nil {
					lists = append(lists, *spec.DeniedDomains)
				}
				if p := run.WallPreset; p != nil && p.Spec.DeniedDomains != nil {
					lists = append(lists, *p.Spec.DeniedDomains)
				}
			}
			denies[ap.RunID] = lists
		}
		if slices.ContainsFunc(lists, func(d []string) bool { return ceilingDenies(d, approvalHost(ap)) }) {
			rows[i].WallsBlock = true
		}
	}
}
