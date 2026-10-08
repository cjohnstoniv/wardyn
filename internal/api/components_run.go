// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runComponents is what the component gate decided about one run's
// components. The zero value means "no components": it caps nothing, floors
// nothing and records nothing, and it is what every lane that runs no
// component gate passes (record, scan, verify, revive).
type runComponents struct {
	// attached is every component the run carries, in request order.
	attached []attachedComponent
	// selfDefined counts the attached components the launcher defined: inline,
	// or a saved one they own. An organisation's component never counts.
	selfDefined int
	// settings is the site config's components block, as the gate read it.
	settings types.ComponentSettings
	// mitmHosts is the bare host of every header delivery. The proxy injects a
	// header only inside a connection it terminates, so dispatch intercepts
	// each of these on its standard TLS port; it is also how the confinement
	// floor tells a component's credential from any other (confinementFloorSpec).
	mitmHosts []string
	// configEnv is the components' plain, non-secret environment. Dispatch
	// writes it into the sandbox, never over a variable the platform set.
	configEnv map[string]string
}

// How an attached component came to be on the run.
const (
	componentSourceOrg    = "org"    // an organisation's row, by id
	componentSourceSelf   = "self"   // the launcher's own saved row, by id
	componentSourceInline = "inline" // defined on the request, for this run only
)

// attachedComponent is one component the gate admitted.
type attachedComponent struct {
	// snapshot is the run_components row: a copy of the definition as
	// launched, who defined it, and — through SelfDefined and ComponentID —
	// the door a revive re-checks.
	snapshot types.RunComponent
	source   string
	// addedHosts is what this component added to the run's allowed domains.
	addedHosts []string
	// needsOwnSecrets and needsAdminSecret are set on the policy preview alone,
	// where a secret that is not stored yet is a fact to show rather than a
	// refusal; the other two doors refuse both. needsOwnSecrets names, in the
	// definition's order, each secret of their own the caller has still to
	// store. needsAdminSecret says the organisation has not stored one it
	// provides — never which: that name is the operator's.
	needsOwnSecrets  []string
	needsAdminSecret bool
}

// componentAdmission is one request's state while its components are admitted
// in order: what a later component may not repeat from an earlier one.
type componentAdmission struct {
	// caller is the run identity's subject: whose saved components the request
	// may name, and whose own secrets a component may deliver.
	caller      string
	settings    types.ComponentSettings
	bounds      componentHostBounds
	resident    map[string]bool
	credentials bool
	// operatorSecrets is the operator namespace's names, listed at most once.
	operatorSecrets map[string]bool
}

// componentRefusal is a gate refusal and what its audit row may say about the
// component it concerns (ordinal -1: the list itself).
type componentRefusal struct {
	refusal *runRefusal
	reason  string
	ordinal int
	source  string
	id      *uuid.UUID
}

// applyRunComponents is the component gate, the one place a run's components
// become policy. A component enforces nothing itself: the gate bounds each one
// and then expands it into primitives every later stage already enforces —
// allowed domains and eligible grants on spec, which it edits in place.
//
// All three run doors call it at the same point: after the workspace and
// direct-GitHub folds, so a host an admin already credentialed is seen and a
// component naming it is refused; and before the confinement floor, the
// model-provider choice and the autonomy grade, which must read the expanded
// spec. credentials is false on the policy preview alone, which reports a
// secret that is not stored yet — the caller's own, or one the organisation
// provides — rather than refusing it.
//
// It refuses and names the item, where the member's policy pipeline drops: an
// inline policy is a wish-list, a component is a selection the person can
// edit. A refused launch is audited (run.component.refuse); a refused dry run
// is not.
func (s *Server) applyRunComponents(r *http.Request, req createRunRequest, spec *types.RunPolicySpec,
	ceiling governanceCeiling, wsRefs []types.Workspace, credentials bool,
) (runComponents, *runRefusal) {
	// One credential per host, for every run, before any component is looked
	// at: the policy's own credentials and the deployment's (credentialHostRefusal).
	if refusal := s.credentialHostRefusal(r, *spec); refusal != nil {
		return runComponents{}, refusal
	}
	if len(req.Components) == 0 {
		return runComponents{}, nil
	}
	// One capability snapshot and one listing of the caller's own secrets for
	// the whole list, however many hosts and secrets it names.
	r = r.WithContext(withOwnedSecretMemo(withCapBatch(r.Context())))
	comps, refused := s.admitRunComponents(r, req.Components, *spec, ceiling, wsRefs, credentials)
	if refused != nil {
		s.auditComponentRefusal(r, *refused)
		return runComponents{}, refused.refusal
	}
	comps.expand(spec)
	return comps, nil
}

// admitRunComponents decides every component before any is expanded, so a
// refusal leaves the spec untouched.
func (s *Server) admitRunComponents(r *http.Request, refs []types.ComponentRef, spec types.RunPolicySpec,
	ceiling governanceCeiling, wsRefs []types.Workspace, credentials bool,
) (runComponents, *componentRefusal) {
	ctx := r.Context()
	if refused := componentRefsShape(refs); refused != nil {
		return runComponents{}, refused
	}
	st, ok := s.cfg.Store.(store.ComponentStore)
	if !ok {
		// Never drop the components and carry on: the run would launch without
		// what was asked for, and with no record of the doors it needed.
		return runComponents{}, &componentRefusal{ordinal: -1, reason: reasonComponentStoreUnavailable, refusal: runError(
			http.StatusNotImplemented, reasonComponentStoreUnavailable, "components require the Postgres store backend")}
	}
	adm := componentAdmission{caller: runIdentitySubject(ctx, principalFromRequest(r)), resident: residentTargets(spec), credentials: credentials}
	var ids []uuid.UUID
	for _, ref := range refs {
		if ref.ID != nil {
			ids = append(ids, *ref.ID)
		}
	}
	stored := map[uuid.UUID]types.Component{}
	if len(ids) > 0 {
		rows, err := st.ListComponentsByIDs(ctx, adm.caller, ids)
		if err != nil {
			return runComponents{}, &componentRefusal{refusal: runServerError("list components", err)}
		}
		for _, row := range rows {
			stored[row.ID] = row
		}
	}
	// One read of the site config for the whole gate: the organisation's
	// component settings, its model providers and its redirects. Fail closed —
	// bounds computed from a config nobody could read would admit a model host.
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return runComponents{}, &componentRefusal{refusal: runServerError("get site config", err)}
	}
	adm.settings = componentSettings(sc)
	adm.bounds = s.newComponentHostBounds(r, sc, spec, ceiling, wsRefs)

	comps := runComponents{settings: adm.settings}
	for i, ref := range refs {
		c, refused := s.admitComponent(r, &adm, i, ref, stored)
		if refused != nil {
			return runComponents{}, refused
		}
		if c.snapshot.SelfDefined {
			comps.selfDefined++
		}
		comps.attached = append(comps.attached, c)
	}
	return comps, nil
}

// componentRefsShape is the gate's first step: at most MaxComponentRefs
// entries, each naming exactly one of a stored component and an inline
// definition, and no stored component twice. An inline definition's own shape
// is checked later, after its hosts have been compared: here it is swapped for
// an empty one so the reference's rule can run without it.
func componentRefsShape(refs []types.ComponentRef) *componentRefusal {
	invalid := func(ordinal int, msg string) *componentRefusal {
		return &componentRefusal{ordinal: ordinal, reason: reasonComponentRefInvalid,
			refusal: runError(http.StatusBadRequest, reasonComponentRefInvalid, msg)}
	}
	if len(refs) > types.MaxComponentRefs {
		return invalid(-1, fmt.Sprintf("components: %d entries exceeds the %d-component limit", len(refs), types.MaxComponentRefs))
	}
	for i, ref := range refs {
		shape := ref
		if shape.Inline != nil {
			shape.Inline = &types.ComponentDefinition{}
		}
		if err := shape.Validate(proxy.ValidDomainEntry); err != nil {
			return invalid(i, fmt.Sprintf("components[%d]: %v", i, err))
		}
		if ref.ID == nil {
			continue
		}
		if j := slices.IndexFunc(refs[:i], func(o types.ComponentRef) bool { return o.ID != nil && *o.ID == *ref.ID }); j >= 0 {
			return invalid(i, fmt.Sprintf("components[%d]: the same component as components[%d]", i, j))
		}
	}
	return nil
}

// admitComponent takes one reference through the gate's steps, in order.
func (s *Server) admitComponent(r *http.Request, adm *componentAdmission, i int, ref types.ComponentRef,
	stored map[uuid.UUID]types.Component,
) (attachedComponent, *componentRefusal) {
	// Who it is and who may attach it: answered before anything about its
	// content, so a person who may not attach a component learns nothing of it.
	c, refused := s.resolveComponentRef(r, adm, i, ref, stored)
	if refused != nil {
		return attachedComponent{}, refused
	}
	def := c.snapshot.Definition
	// A model's host first, on the parsed destination and before the shape
	// check: a spelling variant of a model host is refused for what it
	// reaches, not for how it is spelled.
	for hi, h := range def.Hosts {
		if d, err := types.ParseDestination(h); err == nil && adm.bounds.servesModel(d) {
			return attachedComponent{}, componentRefused(c, i, reasonComponentHostServesModel,
				fmt.Sprintf("hosts[%d]: %q serves a model on this deployment, and a component may not reach a model's host", hi, h),
				"reaches a host that serves a model, which a component may not.")
		}
	}
	if problem := componentDefinitionProblem(c); problem != "" {
		return attachedComponent{}, componentRefused(c, i, reasonComponentDefinitionInvalid, problem, "has a definition that is not valid here.")
	}
	if refused := s.componentSecretsOwned(r, adm, &c, i); refused != nil {
		return attachedComponent{}, refused
	}
	if refused := s.componentHostsBounded(r, adm, c, i); refused != nil {
		return attachedComponent{}, refused
	}
	if si := componentResidentSecret(def); si >= 0 && adm.settings.DenyResidentDelivery {
		return attachedComponent{}, componentRefused(c, i, reasonComponentResidentDeliveryDenied,
			fmt.Sprintf("secrets[%d].delivery.mode: your organisation has turned off delivering a secret into the sandbox as a variable or a file; deliver it as a header", si),
			"delivers a secret into the sandbox, which your organisation has turned off.")
	}
	problem := componentModelEnvProblem(def)
	if problem == "" {
		problem = claimResidentTargets(def, adm.resident)
	}
	if problem != "" {
		return attachedComponent{}, componentRefused(c, i, reasonComponentDefinitionInvalid, problem, "has a definition that is not valid here.")
	}
	return s.componentSharedSecretsPresent(r, adm, c, i)
}

// resolveComponentRef finds the component a reference names and asks whether
// the caller may attach it.
//
// An id that names no row the caller may see — absent, or another person's —
// answers with the very bytes componentAttachRefusal gives a person who is not
// granted an organisation's component: never a 404, so whether an id exists is
// not observable. The attach door is asked first, so a refusal it would give
// anyway (the kind enforced, or a deleted component's restriction kept) comes
// from it.
func (s *Server) resolveComponentRef(r *http.Request, adm *componentAdmission, i int, ref types.ComponentRef,
	stored map[uuid.UUID]types.Component,
) (attachedComponent, *componentRefusal) {
	c := attachedComponent{source: componentSourceInline, snapshot: types.RunComponent{SelfDefined: true, Owner: adm.caller, Name: ref.Name}}
	door := ""
	if ref.Inline != nil {
		c.snapshot.Definition = *ref.Inline
	} else {
		row, found := stored[*ref.ID]
		if !found {
			refusal := s.componentAttachRefusal(r, ref.ID.String())
			if refusal == nil {
				refusal = runDenied(authz.Deny(authz.ReasonCapabilityComponent, "runs.component", componentUnavailableRefusal))
			}
			return attachedComponent{}, componentDoorRefusal(refusal, i, "", ref.ID)
		}
		c.source = componentSourceSelf
		if row.Owner == "" {
			c.source, door = componentSourceOrg, ref.ID.String()
		}
		c.snapshot = types.RunComponent{
			SelfDefined: row.Owner != "", ComponentID: ref.ID, Owner: row.Owner,
			Name: row.Name, Version: row.Version, Definition: row.Definition,
		}
	}
	if refusal := s.componentAttachRefusal(r, door); refusal != nil {
		return attachedComponent{}, componentDoorRefusal(refusal, i, c.source, ref.ID)
	}
	return c, nil
}

// componentDoorRefusal wraps the attach door's answer with what the refusal's
// audit row records.
func componentDoorRefusal(refusal *runRefusal, ordinal int, source string, id *uuid.UUID) *componentRefusal {
	out := &componentRefusal{refusal: refusal, ordinal: ordinal, source: source, id: id}
	if refusal.decision != nil {
		out.reason = string(refusal.decision.Reason)
	}
	return out
}

// componentRefused is a 422 about component i. It names the field at fault for a
// component the person defined — they typed it. For an organisation's
// component it says only what kind of problem "this component" has: its
// content is the admin's, and the person can do nothing with a field name.
func componentRefused(c attachedComponent, i int, reason, problem, orgProblem string) *componentRefusal {
	msg := fmt.Sprintf("components[%d]: %s%s", i, c.fieldPath(), problem)
	if !c.snapshot.SelfDefined {
		msg = fmt.Sprintf("components[%d]: this component %s Ask your admin.", i, orgProblem)
	}
	return &componentRefusal{
		refusal: runError(http.StatusUnprocessableEntity, reason, msg),
		reason:  reason, ordinal: i, source: c.source, id: c.snapshot.ComponentID,
	}
}

// fieldPath is where a field of the component's definition sits, as the
// person addressed it: inline on the request, or in their saved row.
func (c attachedComponent) fieldPath() string {
	if c.snapshot.ComponentID == nil {
		return "inline."
	}
	return "definition."
}

// componentHostsBounded holds every destination of component i to the
// organisation's outer wall — its deny lists and its egress_host capability
// rows — and every header delivery to one credential per host.
//
// The allowlist is deliberately not consulted: a component may reach anything
// the organisation has not blocked. The denies are the backstop, here and
// again at dispatch.
func (s *Server) componentHostsBounded(r *http.Request, adm *componentAdmission, c attachedComponent, i int) *componentRefusal {
	def := c.snapshot.Definition
	for hi, h := range def.Hosts {
		// Validate parsed every host, so a parse failure here is a host nothing
		// compared: refused with the rest, never skipped.
		d, err := types.ParseDestination(h)
		allowed, cerr := s.capSeamAllowed(r.Context(), capEgressHost, h)
		if cerr != nil {
			return &componentRefusal{refusal: runServerError("resolve capability", cerr)}
		}
		if err != nil || !allowed || adm.bounds.denied(d) {
			return componentRefused(c, i, reasonComponentHostDenied,
				fmt.Sprintf("hosts[%d]: %q is blocked by your organisation", hi, h), "reaches a host that is blocked for you.")
		}
	}
	for si, sec := range def.Secrets {
		if sec.Delivery.Mode != types.ComponentDeliveryHeader {
			continue
		}
		d, err := types.ParseDestination(sec.Delivery.Host)
		if err != nil || adm.bounds.collides(d) {
			return componentRefused(c, i, reasonComponentHostCollision,
				fmt.Sprintf("secrets[%d].delivery.host: %q already has a credential on this run, and a host carries only one", si, sec.Delivery.Host),
				"delivers a credential to a host that already has one on this run.")
		}
		adm.bounds.credentialed = append(adm.bounds.credentialed, d)
	}
	return nil
}

// expand turns the admitted components into the primitives policy already
// enforces, on spec: their hosts into its allowed domains, each secret into
// the grant of its delivery, and their config into configEnv.
func (c *runComponents) expand(spec *types.RunPolicySpec) {
	for i := range c.attached {
		a := &c.attached[i]
		def := a.snapshot.Definition
		a.addedHosts = unionAllowedDomains(spec, def.Hosts)
		for _, sec := range def.Secrets {
			spec.EligibleGrants = append(spec.EligibleGrants, componentGrant(sec))
			if sec.Delivery.Mode == types.ComponentDeliveryHeader {
				c.mitmHosts = append(c.mitmHosts, sec.Delivery.Host)
			}
		}
		for k, v := range def.Config {
			if c.configEnv == nil {
				c.configEnv = map[string]string{}
			}
			c.configEnv[k] = v
		}
	}
}

// componentGrant is the grant one component secret becomes. Every grant for a
// person's own secret is owner_only, which closes the read's fallback to the
// operator's row of the same name: no operator material reaches a run through
// a component except a `shared` header credential, which the sink reads from
// the operator's namespace and nowhere else (injectionGrantRead).
func componentGrant(sec types.ComponentSecret) types.GrantSpec {
	del := sec.Delivery
	switch del.Mode {
	case types.ComponentDeliveryEnv:
		return types.GrantSpec{Kind: types.GrantEnvSecret, OwnerOnly: true,
			Scope: mustJSON(map[string]string{"name": del.Var, "secret_name": sec.SecretName})}
	case types.ComponentDeliveryFile:
		return types.GrantSpec{Kind: types.GrantFileSecret, OwnerOnly: true,
			Scope: mustJSON(map[string]string{"file": del.File, "secret_name": sec.SecretName})}
	}
	// An empty header or format takes injectionRuleFromScope's default.
	scope := map[string]any{"host": del.Host, "secret_name": sec.SecretName, "require_tls": !del.PlainHTTP}
	if del.Header != "" {
		scope["header"] = del.Header
	}
	if del.Format != "" {
		scope["format"] = del.Format
	}
	if sec.Shared {
		scope["shared"] = true
	}
	return types.GrantSpec{Kind: types.GrantAPIKey, Scope: mustJSON(scope), TTLSeconds: 3600, OwnerOnly: !sec.Shared}
}

// confinementFloorSpec is the spec the confinement floor is computed on. A
// credential bound for a host outside the coding-agent baseline floors a run
// to the strongest sandbox; for a COMPONENT's header credential the
// organisation decides whether that holds (components.require_vault_for_credentials,
// off by default), so with it off the copy omits those grants. Every other
// grant, and every other reader of the spec, is untouched.
func confinementFloorSpec(spec types.RunPolicySpec, comps runComponents) types.RunPolicySpec {
	if comps.settings.RequireVaultForCredentials || len(comps.mitmHosts) == 0 {
		return spec
	}
	// One credential per host is the gate's own rule, so the host identifies
	// the component's grant.
	spec.EligibleGrants = slices.DeleteFunc(slices.Clone(spec.EligibleGrants), func(g types.GrantSpec) bool {
		return g.Kind == types.GrantAPIKey && slices.Contains(comps.mitmHosts, apiKeyGrantScopeHost(g.Scope))
	})
	return spec
}

// persistRunComponents writes the run's component snapshot, once: what explain
// reads, and the record of the doors a revive re-checks. A run without
// components writes nothing.
func (s *Server) persistRunComponents(ctx context.Context, runID uuid.UUID, comps runComponents) error {
	if len(comps.attached) == 0 {
		return nil
	}
	st, ok := s.cfg.Store.(store.ComponentStore)
	if !ok {
		return errors.New("api: the store cannot record run components")
	}
	rows := make([]types.RunComponent, len(comps.attached))
	for i, a := range comps.attached {
		rows[i] = a.snapshot
	}
	return st.PutRunComponents(ctx, runID, rows)
}

// auditEntry describes one attached component for an audit row. A row is
// append-only and no erasure reaches it, so a component a PERSON defined is
// described by position, shape and counts alone — never its name, hosts,
// header or secret names; an organisation's is the admin's content and keeps
// its name.
func (a attachedComponent) auditEntry(ordinal int) map[string]any {
	def := a.snapshot.Definition
	secrets := make([]map[string]any, len(def.Secrets))
	for i, sec := range def.Secrets {
		secrets[i] = map[string]any{"delivery": sec.Delivery.Mode, "shared": sec.Shared}
	}
	e := map[string]any{
		"ordinal": ordinal, "kind": types.ComponentCustom, "source": a.source,
		"hosts": len(def.Hosts), "secrets": secrets, "config_keys": len(def.Config),
	}
	if a.snapshot.ComponentID != nil {
		e["component_id"], e["version"] = a.snapshot.ComponentID, a.snapshot.Version
	}
	if !a.snapshot.SelfDefined {
		e["name"] = a.snapshot.Name
	}
	return e
}

// stampRunCreate adds the run's components to the run.create datum, and
// whether any of its reach was added by the launcher. A run without
// components adds neither key.
func (c runComponents) stampRunCreate(data map[string]any) {
	if len(c.attached) == 0 {
		return
	}
	entries := make([]map[string]any, len(c.attached))
	for i, a := range c.attached {
		entries[i] = a.auditEntry(i)
	}
	data["components"], data["self_added_reach"] = entries, c.selfDefined > 0
}

// auditRunComponents records each attached component on its own row, once the
// run exists.
func (s *Server) auditRunComponents(ctx context.Context, runID uuid.UUID, actorType types.ActorType, actor string, comps runComponents) {
	for i, a := range comps.attached {
		s.recordAudit(ctx, s.auditEvent(&runID, actorType, actor, "run.component.attach",
			runID.String(), "success", mustJSON(a.auditEntry(i))))
	}
}

// egressAudit is the data of the run.egress.add row for each component that
// widened the run's allowlist: the hosts for an organisation's component, the
// count alone for a person's (auditEntry's rule).
func (c runComponents) egressAudit() []map[string]any {
	var out []map[string]any
	for i, a := range c.attached {
		if len(a.addedHosts) == 0 {
			continue
		}
		data := map[string]any{"kind": "component", "ordinal": i, "source": a.source, "added_count": len(a.addedHosts)}
		if !a.snapshot.SelfDefined {
			data["added_domains"] = a.addedHosts
		}
		out = append(out, data)
	}
	return out
}

// auditComponentRefusal records a refused launch. A dry run writes nothing —
// Review and the preview re-resolve on every edit — and a server error is not
// a decision about the component. The row carries the reason and where in the
// request the component sat, never what a person typed into it.
func (s *Server) auditComponentRefusal(r *http.Request, refused componentRefusal) {
	if refused.reason == "" || audit.DryRunFrom(r.Context()) {
		return
	}
	data := map[string]any{"reason": refused.reason}
	if refused.ordinal >= 0 {
		data["ordinal"] = refused.ordinal
	}
	if refused.source != "" {
		data["source"] = refused.source
	}
	if refused.id != nil {
		data["component_id"] = refused.id
	}
	actorType, actor := actorFromRequest(r)
	s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor, "run.component.refuse", "", "denied", mustJSON(data)))
}
