// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The image request kind: a person whose default context has an image rule up its unit chain asks for one
// image by digest, a decider the met rule names approves, and the approval writes one exact allow grant of
// that image for the person. It is a user grant, so it holds in any context the person runs in; the digest
// rule, a deny and image admission still bind at every launch. The routes are governance_requests.go's.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// imageRequestPayload is an image request's stored payload.
type imageRequestPayload struct {
	Image   string    `json:"image"`
	UnitID  uuid.UUID `json:"unit_id"` // the default context the offer was read on
	Subject string    `json:"subject"`
}

// imageRequestAfter is the diff's after side.
type imageRequestAfter struct {
	Image    string    `json:"image"`
	UnitID   uuid.UUID `json:"unit_id"`
	UnitName string    `json:"unit_name"`
	Subject  string    `json:"subject"`
}

// imageRequestKey is a request's target key: the person and the digest, so two names for the same bytes
// are one pending request. The grant itself is the exact ref asked for.
func imageRequestKey(subject, ref string) string {
	_, digest, _ := strings.Cut(ref, "@")
	return subject + "|" + digest
}

// proposeImageRequest is the image kind's create.
func proposeImageRequest(s *Server, w http.ResponseWriter, r *http.Request, body governanceRequestBody, who requestCaller) {
	ctx := r.Context()
	ref := strings.TrimSpace(body.Image)
	unit, note, refusal, err := s.imageRequestChecks(ctx, body, ref)
	if err != nil {
		writeServerError(w, r, "resolve capability", err)
		return
	}
	if refusal != nil {
		refusal.write(w)
		return
	}
	payload := imageRequestPayload{Image: ref, UnitID: unit.ID, Subject: types.CanonicalUserSubject(who.principal)}
	s.proposeGovernanceChange(w, r, govProposal{
		kind: types.GovernanceTargetImageRequest, op: "create", key: imageRequestKey(payload.Subject, ref), payload: payload,
		after:   imageRequestAfter{Image: ref, UnitID: unit.ID, UnitName: unit.Name, Subject: payload.Subject},
		changed: []string{"image", "subject", "unit_id"},
		note:    &note,
		answer: func(w http.ResponseWriter, saved types.GovernanceChange) {
			data := map[string]any{"image": ref, "unit_id": unit.ID, "units": []string{unit.ID.String()}, "reason": note.Reason,
				"change_id": saved.ID, "expires_at": saved.ExpiresAt}
			if note.Ticket != "" {
				data["ticket"] = note.Ticket
			}
			actorType, actor := actorFromRequest(r)
			s.recordAudit(ctx, s.auditEvent(nil, actorType, actor, "governance.image_request.request", saved.ID.String(), "success", mustJSON(data)))
			// A pending request reads nothing back: its fields are the payload just written.
			f := requestFields{unitID: &unit.ID, image: ref}
			view := s.requestViewOf(r, saved, "image", types.DeciderKindImage, f, map[uuid.UUID]string{unit.ID: unit.Name})
			writeJSON(w, http.StatusAccepted, map[string]any{"pending_change": view.GovernanceChange, "request": view})
		},
	})
}

// imageRequestChecks is the create's checks in order: only image, reason and ticket; a pinned ref a grant
// can hold; an offer on the default context; the note; a grant that would bind; not already granted. err
// is a capability read's failure only.
func (s *Server) imageRequestChecks(ctx context.Context, body governanceRequestBody, ref string) (types.GovernanceUnit, types.ChangeNote, *elevationRefusal, error) {
	var none types.GovernanceUnit
	if body.UnitID != "" || body.Duration != "" {
		return none, types.ChangeNote{}, &elevationRefusal{status: http.StatusBadRequest, reason: reasonInvalidRequestBody,
			msg: "an image request names only image, reason and ticket"}, nil
	}
	if !prewrappedPinned(ref) || !imageRefPathSafe(ref) {
		return none, types.ChangeNote{}, &elevationRefusal{status: http.StatusBadRequest, reason: reasonImageRequestNotPinned,
			msg: "request an image by digest: <name>@sha256:<64 hex>"}, nil
	}
	if _, err := canonicalGrantValue(capImage, ref); err != nil {
		return none, types.ChangeNote{}, &elevationRefusal{status: http.StatusBadRequest, reason: reasonImageRequestNotPinned,
			msg: "request an image by digest: <name>@sha256:<64 hex>"}, nil
	}
	unit, rule, err := s.imageRequestOffered(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "api: read an image request's offer", "err", err)
		return none, types.ChangeNote{}, &elevationRefusal{status: http.StatusServiceUnavailable, reason: reasonDecidersUnreadable,
			msg: "the deciders rules could not be read; try again"}, nil
	}
	if rule == nil {
		return none, types.ChangeNote{}, &elevationRefusal{status: http.StatusBadRequest, reason: reasonImageRequestNotOffered,
			msg: "your default unit offers no image requests; ask an admin for an image grant"}, nil
	}
	note, refusal := elevationNote(body.Reason, body.Ticket)
	if refusal != nil {
		return none, note, refusal, nil
	}
	binds, err := s.imageGrantWouldBind(ctx, ref)
	if err != nil {
		return none, note, nil, err
	}
	if !binds {
		return none, note, &elevationRefusal{status: http.StatusBadRequest, reason: reasonImageRequestNotOffered,
			msg: "image grants are off in this deployment, so an approved image would not launch; ask an admin"}, nil
	}
	granted, err := s.capGranted(ctx, capImage, ref)
	if err != nil {
		return none, note, nil, err
	}
	if granted {
		return none, note, &elevationRefusal{status: http.StatusConflict, reason: reasonImageAlreadyGranted, msg: ref + " is already granted to you"}, nil
	}
	return unit, note, nil, nil
}

// imageRequestOffered is the unit an image request is offered on and its met image rule: the caller's
// default context (an empty choice, so a laxer unit the person may pick is never asked) and the meet of
// every image rule from it up (requestRuleFor's, on the same tree). No rule when no unit holds an image
// rule (and then no membership is read), for Default, for a chain with none, and for a context the caller
// cannot be placed in (a stale or truncated group snapshot, a choice refusal, an unknown user type): those
// are denials. err is a store or deciders-tree read error only. It is a display read, so it records no
// refusal row.
func (s *Server) imageRequestOffered(ctx context.Context) (types.GovernanceUnit, *types.DeciderRule, error) {
	ctx = withDisplayRead(ctx)
	tree, err := s.deciderTree(ctx)
	if err != nil {
		return types.GovernanceUnit{}, nil, fmt.Errorf("%w: %w", errDecidersRead, err)
	}
	if !imageRuleAnywhere(tree) {
		return types.GovernanceUnit{}, nil, nil
	}
	subj, err := s.callerSubjects(ctx)
	if errors.Is(err, errUserTypeUnknown) {
		return types.GovernanceUnit{}, nil, nil
	}
	if err != nil {
		return types.GovernanceUnit{}, nil, err
	}
	// A stale or truncated snapshot is a denial whatever the pick would be, as elevationEligible reads it,
	// so no groups-stale refusal is decided here.
	if subj.stale || oidcGroupsTruncatedFromContext(ctx) {
		return types.GovernanceUnit{}, nil, nil
	}
	q := contextQuery{users: subj.users, groups: subj.groups, userType: subj.userType}
	pick, err := s.selectContext(ctx, q, contextChoice{})
	if contextRunRefusal(err) != nil {
		return types.GovernanceUnit{}, nil, nil
	}
	if err != nil {
		return types.GovernanceUnit{}, nil, err
	}
	if pick.isDefault() {
		return pick.unit, nil, nil
	}
	rule, _, ok := tree.ruleFor(pick.unit.ID, types.DeciderKindImage)
	if !ok {
		return pick.unit, nil, nil
	}
	return pick.unit, &rule, nil
}

// imageRuleAnywhere is whether any unit of t holds an image rule.
func imageRuleAnywhere(t *deciderTree) bool {
	if t == nil {
		return false
	}
	for _, rules := range t.rules {
		if slices.ContainsFunc(rules, func(r types.DeciderRule) bool { return r.Names(types.DeciderKindImage) }) {
			return true
		}
	}
	return false
}

// imageGrantWouldBind is whether an allow grant of ref could decide a launch now: the image kind's switch
// is on, or ref is a restricted value (capRead.enforced's rule). Off, an approved grant would sit inert
// and the launch would still refuse, so the request is not offered. No store: no grant binds.
func (s *Server) imageGrantWouldBind(ctx context.Context, ref string) (bool, error) {
	b := s.capBatchFor(ctx)
	if b.noStore {
		return false, nil
	}
	on, err := b.enforced(ctx, capImage)
	if err != nil || on {
		return on, err
	}
	return b.isRestricted(ctx, capImage, ref)
}

// imageRequestOfferedQuiet is whether the caller is a person who may ask for an image. A read error is
// "no": the list's flag and the run refusal's extra clause never fail on it.
func (s *Server) imageRequestOfferedQuiet(r *http.Request) bool {
	if _, ok := s.requester(r); !ok {
		return false
	}
	_, rule, err := s.imageRequestOffered(r.Context())
	if err != nil {
		slog.WarnContext(r.Context(), "api: read an image request's offer", "err", err)
		return false
	}
	return rule != nil
}

// applyImageRequestChange is an approved image request's write: under the grant's own lock, no equal grant
// has appeared and the payload's unit still has an image rule up its chain, and one exact user allow grant
// of the ref is written, created by the decider. Either change would have refused the request: stale.
func applyImageRequestChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	var p imageRequestPayload
	if err := decodeHeldPayload(ch.Payload, &p); err != nil {
		return govApplied{}, fmt.Errorf("api: decode an image request: %w", err)
	}
	_, approver := actorFromRequest(r)
	g := types.CapabilityGrant{ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: p.Subject, Capability: capImage,
		Value: p.Image, Effect: types.CapabilityAllow, CreatedBy: approver}
	if err := store.LockGovernanceTarget(ctx, q, govKindGrant, string(g.SubjectType), g.Subject, g.Capability, g.Value); err != nil {
		return govApplied{}, err
	}
	_, err := store.GetCapabilityGrantByKeyQ(ctx, q, string(g.SubjectType), g.Subject, g.Capability, g.Value, true)
	if err == nil {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if !errors.Is(err, store.ErrNotFound) {
		return govApplied{}, err
	}
	rule, _, err := s.requestRuleFor(ctx, q, p.UnitID, types.DeciderKindImage)
	if err != nil {
		return govApplied{}, err
	}
	if rule == nil {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if err := validateCapabilityGrant(&g); err != nil {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonCapabilityGrantInvalid, "invalid grant: %v", err)
	}
	saved, err := store.UpsertCapabilityGrantQ(ctx, q, g)
	if err != nil {
		return govApplied{}, err
	}
	data := map[string]any{"image": saved.Value, "grant_id": saved.ID, "subject_type": saved.SubjectType, "subject": saved.Subject,
		"unit_id": p.UnitID, "units": []string{p.UnitID.String()}}
	if ch.Note != nil && ch.Note.Ticket != "" {
		data["ticket"] = ch.Note.Ticket
	}
	return govApplied{action: "governance.image_request.approve", target: saved.ID.String(), data: data,
		// The decider's scope reads the pool, so it waits for the commit.
		afterCommit: func() map[string]any {
			if scope := s.decisionUnitScope(ctx, p.UnitID, rule); scope != "" {
				return map[string]any{"unit_scope": scope}
			}
			return nil
		}}, nil
}

// imageRequestView is an image request's own fields of a request view: the image and the unit the offer
// was read on.
func imageRequestView(_ *Server, _ context.Context, ch types.GovernanceChange) (requestFields, error) {
	var p imageRequestPayload
	if json.Unmarshal(ch.Payload, &p) != nil {
		return requestFields{}, nil
	}
	return requestFields{unitID: &p.UnitID, image: p.Image}, nil
}
