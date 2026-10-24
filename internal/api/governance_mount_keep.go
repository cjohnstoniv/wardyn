// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The keep-stored guard. A security admin reads every workspace_mounts[].source of a profile as
// mountSourceWithheld (specReadSecurity), so the GET -> edit -> PUT round trip the console and
// `wardyn governance get`/`set` make sends that sentinel back. The guard turns each sentinel back into
// the host path stored at the same target, and lets only a super admin (isOperator, the tier that
// writes the host) send a literal source or make a kept mount writable.
//
// It runs inside buildGovernanceUnit, so a direct write, a held write at proposal and its replay
// at approval are judged by the same code, each with the tier of the request it serves.

// mountKeep is what the guard decided for one write.
type mountKeep struct {
	// setsHostPath: the write adds a mount, changes a mount's host path or makes a kept mount
	// writable. Only a super admin may decide such a change (governanceChangeKinds forChange).
	setsHostPath bool
	// ceiling and overlay are the request's mounts as a held change stores them: the sentinel where a
	// mount is kept as stored, the literal host path only where the write sets one. nil leaves the
	// request's own value.
	ceiling []types.WorkspaceMount
	overlay *[]types.WorkspaceMount
}

// mountTierRefusal is the guard's 403: a caller below the super admin tier sent a host path, or
// made a kept mount writable.
type mountTierRefusal struct {
	field    string // ceiling.workspace_mounts[i].source, or .read_only
	sentence string
}

func (e *mountTierRefusal) Error() string {
	return string(authz.ReasonAdminSurface) + ": " + e.sentence
}

func (e *mountTierRefusal) decision() authz.Decision {
	return authz.Deny(authz.ReasonAdminSurface, "governance.unit."+e.field, e.sentence)
}

// keepMounts is the guard over one list of mounts. keep is the stored mounts, earlier entries first
// (a target's first entry is the one kept). It returns the mounts to store, and their held form.
//
// A mount is matched by target, never by position, so a reorder keeps each host path with its own
// target; targets are unique, which validatePolicyWorkspaces enforces after this. A mount left out is
// dropped, and nothing here puts one back.
func keepMounts(field string, in, keep []types.WorkspaceMount, super bool) (out, held []types.WorkspaceMount, sets bool, err error) {
	if len(in) == 0 {
		return in, in, false, nil
	}
	stored := make(map[string]types.WorkspaceMount, len(keep))
	for _, k := range keep {
		if _, seen := stored[k.Target]; !seen {
			stored[k.Target] = k
		}
	}
	out, held = make([]types.WorkspaceMount, len(in)), make([]types.WorkspaceMount, len(in))
	for i, m := range in {
		k, known := stored[m.Target]
		writable := known && k.ReadOnlyOrDefault() && !m.ReadOnlyOrDefault()
		out[i], held[i] = m, m
		switch {
		case m.Source == mountSourceWithheld && !known:
			return nil, nil, false, writeRefusal(http.StatusUnprocessableEntity, reasonGovernanceMountKeepUnknown,
				"%s[%d]: %s has no stored host path to keep; a new mount needs its host path, and only a super admin sets one",
				field, i, m.Target)
		case m.Source == mountSourceWithheld:
			out[i].Source = k.Source
			if writable {
				if !super {
					return nil, nil, false, &mountTierRefusal{field: field + "[" + strconv.Itoa(i) + "].read_only", sentence: field + "[" + strconv.Itoa(i) +
						"].read_only: only a super admin may make a mount writable; keep it read-only or remove it"}
				}
				sets, held[i].Source = true, k.Source
			}
		case !super:
			// Refused even when it equals the stored source: a 200 there would answer whether a guessed
			// host path is the stored one.
			return nil, nil, false, &mountTierRefusal{field: field + "[" + strconv.Itoa(i) + "].source", sentence: field + "[" + strconv.Itoa(i) +
				"].source: only a super admin may set a mount's host path; send the mount back as you read it, or remove it"}
		case known && m.Source == k.Source && !writable:
			held[i].Source = mountSourceWithheld // a super admin's round trip of a stored mount keeps it
		default:
			sets = true
		}
	}
	return out, held, sets, nil
}

// storedCeilingMounts is the keep-set of a standalone write to profile id: the stored row's ceiling
// mounts, or, for a row composed now (the console folds one into a standalone ceiling), its effective
// ceiling's. A create keeps nothing.
func (s *Server) storedCeilingMounts(all []types.GovernanceUnit, id uuid.UUID) []types.WorkspaceMount {
	cur, ok := unitsByID(all)[id]
	switch {
	case !ok:
		return nil
	case !cur.Composed():
		return cur.Ceiling.WorkspaceMounts
	}
	if res := s.resolveForDiff(all, id); res != nil {
		return res.Ceiling.WorkspaceMounts
	}
	return nil
}

// storedOverlayMounts is the keep-set of an overlay write to profile id on base: the stored overlay's
// mounts first, then the mounts the base resolves to (the deployment default's when there is no base).
// Stored first, so a stored overlay mount is never paired with a host path a super admin has since
// given the base at that target.
func (s *Server) storedOverlayMounts(all []types.GovernanceUnit, id uuid.UUID, base *uuid.UUID) []types.WorkspaceMount {
	var keep []types.WorkspaceMount
	byID := unitsByID(all)
	if cur, ok := byID[id]; ok && cur.Overlay != nil && cur.Overlay.WorkspaceMounts != nil {
		keep = append(keep, *cur.Overlay.WorkspaceMounts...)
	}
	if base == nil {
		return append(keep, s.deploymentAuthority().Ceiling.WorkspaceMounts...)
	}
	if res := s.resolveForDiff(all, *base); res != nil {
		keep = append(keep, res.Ceiling.WorkspaceMounts...)
	}
	return keep
}

// scrubMountSources replaces every host path in sources with mountSourceWithheld in a refusal's
// message, so a caller below the super admin tier is not told a stored path by the refusal either.
func scrubMountSources(err error, sources []string) error {
	var refusal *unitWriteError
	if !errors.As(err, &refusal) {
		return err
	}
	msg := refusal.msg
	for _, src := range sources {
		if src != "" && src != mountSourceWithheld {
			msg = strings.ReplaceAll(msg, src, mountSourceWithheld)
		}
	}
	return &unitWriteError{status: refusal.status, reason: refusal.reason, msg: msg}
}

// mountSourcesOf is every host path the profile graph and the deployment default name: what a
// refusal to a caller below the super admin tier must not echo.
func (s *Server) mountSourcesOf(all []types.GovernanceUnit) []string {
	var out []string
	add := func(ms []types.WorkspaceMount) {
		for _, m := range ms {
			out = append(out, m.Source)
		}
	}
	add(s.deploymentAuthority().Ceiling.WorkspaceMounts)
	for _, p := range all {
		add(p.Ceiling.WorkspaceMounts)
		if p.Overlay != nil && p.Overlay.WorkspaceMounts != nil {
			add(*p.Overlay.WorkspaceMounts)
		}
	}
	return out
}

// heldProfilePayload is the payload a held profile write stores: the request, with each mount in the
// form keepMounts held it, so a payload names a host path only where the write sets one.
func heldProfilePayload(req governanceUnitRequest, keep mountKeep) unitChangePayload {
	p := unitPayloadOf(req)
	if keep.ceiling != nil {
		p.Ceiling.WorkspaceMounts = keep.ceiling
	}
	if keep.overlay != nil {
		var fields map[string]json.RawMessage
		if json.Unmarshal(req.Overlay, &fields) == nil {
			for k := range fields {
				if strings.EqualFold(k, "workspace_mounts") {
					delete(fields, k)
				}
			}
			fields["workspace_mounts"] = mustJSON(*keep.overlay)
			p.Overlay = mustJSON(fields)
		}
	}
	return p
}

// profilePayloadSetsHostPath reports whether a held profile change names any literal mount source,
// in its ceiling or its overlay. A payload that does not decode is treated as one that does.
func profilePayloadSetsHostPath(raw json.RawMessage) bool {
	var held struct {
		Ceiling struct {
			WorkspaceMounts []types.WorkspaceMount `json:"workspace_mounts"`
		} `json:"ceiling"`
		Overlay *struct {
			WorkspaceMounts []types.WorkspaceMount `json:"workspace_mounts"`
		} `json:"overlay"`
	}
	if json.Unmarshal(raw, &held) != nil {
		return true
	}
	mounts := held.Ceiling.WorkspaceMounts
	if held.Overlay != nil {
		mounts = append(mounts, held.Overlay.WorkspaceMounts...)
	}
	for _, m := range mounts {
		if m.Source != mountSourceWithheld {
			return true
		}
	}
	return false
}

// changeForRead is a served change with its diff as the reader may see it. Below the super admin tier
// the diff carries no belongs_by, the units its proposer was granted that the subject holds another
// membership below: a reader whose scope covers only the target would learn of a membership under a
// unit they do not govern. The decision reads the stored diff, not this one (heldBelongsStale). Below
// that tier too every workspace_mounts[].source anywhere in the diff is mountSourceWithheld. A change
// that sets a host path is already hidden from that tier (canSeeGovernanceChange); this holds whatever the diff
// was rendered with. At the member tier (a unit admin) every ceiling in it also passes
// redactSpecForUser and every overlay redactOverlayForUser, the step each read redaction adds there
// over the security tier the diff was rendered at, so a held change's specs read as GET /governance
// shows them. Both sides of the diff
// pass it alike and changed keeps the paths the rendered views differ at, so a redacted field never
// reads as a change it is not. A diff that does not decode is not served.
func (s *Server) changeForRead(r *http.Request, ch types.GovernanceChange) types.GovernanceChange {
	tier := s.specReadTierFor(r.Context())
	if tier == specReadFull || len(ch.Diff) == 0 {
		return ch
	}
	dec := json.NewDecoder(strings.NewReader(string(ch.Diff)))
	dec.UseNumber()
	var doc any
	if dec.Decode(&doc) != nil || (tier == specReadMember && !redactCeilingsForUserIn(doc)) {
		ch.Diff = json.RawMessage("null")
		return ch
	}
	if body, ok := doc.(map[string]any); ok {
		delete(body, "belongs_by")
	}
	withholdMountSourcesIn(doc)
	ch.Diff = mustJSON(doc)
	return ch
}

// redactCeilingsForUserIn replaces every "ceiling" object in v (each one a types.RunPolicySpec) with
// redactSpecForUser of it and every "overlay" object (a types.CeilingOverlay) with
// redactOverlayForUser of it, and reports false when one does not decode as its type.
func redactCeilingsForUserIn(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			m, ok := val.(map[string]any)
			switch {
			case ok && k == "ceiling":
				var spec types.RunPolicySpec
				if json.Unmarshal(mustJSON(m), &spec) != nil {
					return false
				}
				x[k] = mustJSON(redactSpecForUser(spec))
			case ok && k == "overlay":
				var o types.CeilingOverlay
				if json.Unmarshal(mustJSON(m), &o) != nil {
					return false
				}
				x[k] = mustJSON(redactOverlayForUser(&o))
			case !redactCeilingsForUserIn(val):
				return false
			}
		}
	case []any:
		for _, e := range x {
			if !redactCeilingsForUserIn(e) {
				return false
			}
		}
	}
	return true
}

func withholdMountSourcesIn(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if list, ok := val.([]any); ok && strings.EqualFold(k, "workspace_mounts") {
				for _, e := range list {
					if m, ok := e.(map[string]any); ok {
						if _, has := m["source"]; has {
							m["source"] = mountSourceWithheld
						}
					}
				}
				continue
			}
			withholdMountSourcesIn(val)
		}
	case []any:
		for _, e := range x {
			withholdMountSourcesIn(e)
		}
	}
}

// withoutKeptMounts is spec without the mounts whose source is the sentinel, for the decode check
// that runs before the stored host paths are put back.
func withoutKeptMounts(spec types.RunPolicySpec) types.RunPolicySpec {
	spec.WorkspaceMounts = slices.DeleteFunc(slices.Clone(spec.WorkspaceMounts),
		func(m types.WorkspaceMount) bool { return m.Source == mountSourceWithheld })
	return spec
}
