// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The support arm: how a door asks whether a helper the run's owner accepted may enter. A support grant is one
// row naming a run, a helper, the doors the owner ticked and an expiry on the database's clock
// (store_support_grants.go). The owner rule (mayEnterRun, getRunForEntry, admitRunEntry) is not changed:
// supportMayEnter is a second question, asked only by the entry points the support census names, and a helper
// always enters governed, so no support entry ever takes adminDoorExempt.

// supportDoor is a door the owner may tick, or one of the two pseudo-doors a caller that cannot tell which of
// several doors it serves asks for.
type supportDoor string

const (
	supportDoorTerminal supportDoor = "terminal" // a web terminal; an SSH shell
	supportDoorCommands supportDoor = "commands" // SSH exec; POST /exec
	supportDoorFiles    supportDoor = "files"    // sftp and sync; the Files panel
	supportDoorForward  supportDoor = "forward"  // ssh -L
	supportDoorWebApps  supportDoor = "web_apps" // the UI gateway's apps that are not a desktop
	supportDoorDesktop  supportDoor = "desktop"  // a declared noVNC or a VM's display
	// supportDoorSSH passes when any of the first four is ticked: an SSH connection's auth does not yet know
	// which channel it will open. supportDoorUI passes when web_apps or desktop is ticked.
	supportDoorSSH supportDoor = "ssh"
	supportDoorUI  supportDoor = "ui"
)

// supportDoorNames is the six doors an owner may tick, in the order the console shows them.
var supportDoorNames = []string{string(supportDoorTerminal), string(supportDoorCommands), string(supportDoorFiles),
	string(supportDoorForward), string(supportDoorWebApps), string(supportDoorDesktop)}

// supportPseudoDoors is what each pseudo-door stands for.
var supportPseudoDoors = map[supportDoor][]string{
	supportDoorSSH: supportDoorNames[:4],
	supportDoorUI:  supportDoorNames[4:],
}

// supportDoorTicked reports whether door, or for a pseudo-door any door it stands for, is in doors.
func supportDoorTicked(doors []string, door supportDoor) bool {
	if members, ok := supportPseudoDoors[door]; ok {
		return slices.ContainsFunc(members, func(m string) bool { return slices.Contains(doors, m) })
	}
	return slices.Contains(doors, string(door))
}

// supportVerdict is supportMayEnter's answer. The zero value is supportNone, so a verdict nobody set refuses.
type supportVerdict int

const (
	// supportNone: no grant for this entry, or the arm does not apply. The caller answers with its own
	// unchanged refusal (the owner rule's run_owner_only).
	supportNone supportVerdict = iota
	// supportOK: the helper holds a live grant that ticks the door.
	supportOK
	// supportEnded: the entry named a grant that is no longer live.
	supportEnded
	// supportDoorRefused: a live grant that does not tick the door.
	supportDoorRefused
	// supportOff: WARDYN_SUPPORT_SESSIONS is off.
	supportOff
	// supportUnavailable: the store keeps no grants or could not be read. Every caller refuses (fail closed).
	supportUnavailable
)

// supportPerson is the signed-in person behind ctx when that sign-in may ask for help or answer a request: a
// console session, or a CLI sign-in's token. A portal's delegated request, a personal API token alone, the
// admin token and local mode (no person) are "".
func supportPerson(ctx context.Context) string {
	if delegatedRequest(ctx) || (apiTokenIDFromContext(ctx) != uuid.Nil && cliSessionIDFromContext(ctx) == uuid.Nil) {
		return ""
	}
	return oidcHumanFromContext(ctx)
}

// supportCaller reports whether the caller on ctx may ask to help inside a run: a super admin signed in as a
// person (isOperator is false for a device, a portal and a service; it reads true for the admin token, which
// supportPerson then refuses). That the caller is not the run's own creator is the route's check.
func (s *Server) supportCaller(ctx context.Context) bool {
	return s.isOperator(ctx) && supportPerson(ctx) != ""
}

// The kinds of run a grant cannot be made on, as supportRunIneligible names them.
const (
	supportIneligibleLocalMode  = "local_mode"
	supportIneligibleNotRunning = "not_running"
	supportIneligibleLost       = "lost"
	supportIneligibleOperator   = "operator_owned"
	supportIneligibleService    = "service"
	supportIneligibleBackground = "background"
	supportIneligibleReserved   = "reserved_task"
)

// supportRunIneligible names why run cannot take a helper, or "" when it can: it must be RUNNING and not lost, a
// person's own (not operator-owned: admins already enter those; not a service identity's: no person to
// consent), not a background task (no person watches it to revoke), not a run of a reserved task (a harness
// sign-in binds the owner's own cloud identity) and not local mode (one seat). Every case reads the run alone, so
// the answer never costs a store read.
func (s *Server) supportRunIneligible(run types.AgentRun) string {
	switch {
	case s.cfg.LocalMode:
		return supportIneligibleLocalMode
	case run.State != types.RunRunning:
		return supportIneligibleNotRunning
	case runIsKept(run):
		return supportIneligibleLost
	case run.OperatorOwned:
		return supportIneligibleOperator
	case sponsoredService(run) != "":
		return supportIneligibleService
	case run.BackgroundOnly():
		return supportIneligibleBackground
	case reservedRunTasks[run.Task]:
		return supportIneligibleReserved
	}
	return ""
}

// supportMayEnter asks whether principal may enter run through door as a helper. grant is the grant the
// caller's ticket, session or connection carries, or uuid.Nil for "the open grant naming principal on run".
// The row comes back whenever one was read, so a caller can name its id.
//
// Order: switch off is supportOff; a store without grants is supportUnavailable (both are supportNone when grant
// is Nil, as is the whole arm being absent); an ineligible run, the run's
// own creator, no row, a requested-only row or a row no longer live (ended on the spot) is supportEnded when
// grant is set and supportNone when it is Nil, so a new entry after an end meets the unchanged owner-rule
// refusal; a door the owner did not tick is supportDoorRefused; then two standing checks each end the grant on
// failure: the helper's session was cut (helper_signed_out) or the owner's was revoked (owner_revoked).
func (s *Server) supportMayEnter(ctx context.Context, run types.AgentRun, principal string, grant uuid.UUID, door supportDoor) (store.SupportGrant, supportVerdict) {
	// With no grant named, a switch that is off or a store that keeps none means no support arm at all, so the
	// caller's unchanged owner-rule refusal stands; a ticket, session or connection that names a grant is told.
	off, unavailable := supportNone, supportNone
	gone := supportNone
	if grant != uuid.Nil {
		off, unavailable, gone = supportOff, supportUnavailable, supportEnded
	}
	if !envEnabled(envSupportSessions) {
		return store.SupportGrant{}, off
	}
	st, ok := s.cfg.Store.(store.SupportGrantStore)
	if !ok {
		return store.SupportGrant{}, unavailable
	}
	if principal == "" || run.CreatedBy == principal || s.supportRunIneligible(run) != "" {
		return store.SupportGrant{}, gone
	}
	// An SSH auth that governance then refuses is not presence, so sshAuth alone does not stamp last_seen_at;
	// sshCurrentKey's re-check, which passes the grant id, does.
	stamp := door != supportDoorSSH || grant != uuid.Nil
	g, found, live, err := st.SupportGrantLive(ctx, run.ID, principal, grant, stamp)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: support grant read failed; refusing", "run_id", run.ID, "err", err)
		return store.SupportGrant{}, supportUnavailable
	}
	if !found || !live {
		return g, gone
	}
	if !supportDoorTicked(g.Doors, door) {
		return g, supportDoorRefused
	}
	reason, err := s.supportStandingEnd(ctx, run, g)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: support grant revocation check failed; refusing", "run_id", run.ID, "err", err)
		return g, supportUnavailable
	}
	if reason == "" {
		return g, supportOK
	}
	// audited false: the sweep claims the row and writes its one support.end.
	if _, err := st.EndSupportGrant(ctx, run.ID, g.ID, reason, false); err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.WarnContext(ctx, "wardynd: support grant end failed", "run_id", run.ID, "err", err)
	}
	return g, gone
}

// supportStandingEnd runs the two checks that end a live grant: "helper_signed_out" when the helper's session was
// cut since the request (a sign-out writes CutSessions, which IsRevoked counts), "owner_revoked" when the owner
// was revoked or deactivated since the accept (epoch -1 ignores a session-only cut, so signing out of one
// browser ends no help). "" when neither holds. Without a revocation store there is nothing to ask.
func (s *Server) supportStandingEnd(ctx context.Context, run types.AgentRun, g store.SupportGrant) (string, error) {
	rev := s.cfg.SessionRevocations
	if rev == nil {
		return "", nil
	}
	revoked, err := oidc.IsRevoked(ctx, rev, g.Helper, g.HelperEmail, oidc.Issue{At: g.RequestedAt, Seq: g.RequestRevSeq})
	if err != nil {
		return "", err
	}
	if revoked {
		return supportEndHelperSignedOut, nil
	}
	if g.AcceptedAt == nil {
		return "", nil
	}
	status, err := oidc.CheckSession(ctx, rev, run.CreatedBy, g.OwnerEmail, oidc.Issue{At: *g.AcceptedAt, Seq: g.AcceptRevSeq}, -1)
	if err != nil {
		return "", err
	}
	if status != oidc.SessionLive {
		return supportEndOwnerRevoked, nil
	}
	return "", nil
}

// refuseSupport writes the refusal for v and reports whether it did. supportDoorRefused and supportEnded are 403
// authz denials (support_door_not_granted, support_grant_ended), supportOff is 409 support_disabled and
// supportUnavailable 503 support_unavailable. supportNone and supportOK write nothing: the caller answers with
// its own refusal (supportNone) or goes on (supportOK).
func (s *Server) refuseSupport(w http.ResponseWriter, r *http.Request, run types.AgentRun, v supportVerdict) bool {
	switch v {
	case supportDoorRefused:
		return s.refuse(w, r, authz.Deny(authz.ReasonSupportDoorNotGranted, run.ID.String(), "").OnRun(run.ID))
	case supportEnded:
		return s.refuse(w, r, authz.Deny(authz.ReasonSupportGrantEnded, run.ID.String(), "").OnRun(run.ID))
	case supportOff:
		writeErrorReason(w, http.StatusConflict, reasonSupportDisabled, supportDisabledMsg)
		return true
	case supportUnavailable:
		writeErrorReason(w, http.StatusServiceUnavailable, reasonSupportUnavailable, "support sessions could not be checked right now")
		return true
	}
	return false
}
