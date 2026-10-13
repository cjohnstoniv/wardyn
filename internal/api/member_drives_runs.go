// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Member drives in runs: a person's own drives join their catalogue and
// drives[] through the owner, never through a grant, so no member drive is
// ever the legacy drive. Dispatch re-reads each one right before the sandbox
// is created, and a shared template cannot carry one.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// memberReachableDrives appends the caller's own member drives to out: matched
// on the exact sign-in subject (never the email), each resolved through an
// in-memory user-tier grant to its owner, so its home and object are the ones
// delete reclaims. A drive being deleted is left out. None is legacy.
func (s *Server) memberReachableDrives(ctx context.Context, out []reachableDrive, ceiling driveSizeCeiling) ([]reachableDrive, error) {
	ms, ok := s.cfg.Store.(store.MemberDriveStore)
	owner := types.CanonicalUserSubject(oidcHumanFromContext(ctx))
	if !ok || owner == "" {
		return out, nil
	}
	drives, err := ms.ListMemberDrivesOf(ctx, owner)
	if err != nil {
		return nil, fmt.Errorf("api: list member drives: %w", err)
	}
	for _, d := range drives {
		if d.OwnerSubject != owner || d.DeleteStartedAt != nil {
			continue
		}
		r := reachableDrive{owned: true, winner: store.UserDriveWinner{Drive: d, Grant: types.UserDriveGrant{
			SubjectType: types.CapabilitySubjectUser, Subject: owner, DriveID: d.ID, Enabled: true,
		}}}
		r.resolved, r.err = newResolvedDrive(&r.winner.Drive, &r.winner.Grant, types.CapabilitySubjectUser, []string{owner}, ceiling)
		if r.err != nil && !errors.Is(r.err, errDriveUnmountable) {
			return nil, r.err
		}
		out = append(out, r)
	}
	return out, nil
}

// memberDriveCreateState is the catalogue's create block: nil for a caller who
// is not a person, else create's own doors (memberDriveCreateDoors) and the
// count, read without writing any refusal row.
func (s *Server) memberDriveCreateState(r *http.Request) *client.MemberDriveCreate {
	ctx := r.Context()
	owner := types.CanonicalUserSubject(oidcHumanFromContext(ctx))
	if owner == "" || neverOperator(ctx) {
		return nil
	}
	out := &client.MemberDriveCreate{}
	ms, ok := s.cfg.Store.(store.MemberDriveStore)
	if !ok {
		out.Reason = client.MemberDriveCreateUnsupported
		return out
	}
	mine, err := ms.ListMemberDrivesOf(ctx, owner)
	if err != nil {
		out.Reason = client.MemberDriveCreateGovernance
		return out
	}
	out.Count = len(mine)
	_, policy, refusal := s.memberDriveCreateDoors(r)
	out.Limit = policy.MaxPerPerson
	switch {
	case refusal == nil && out.Count >= out.Limit:
		out.Reason = client.MemberDriveCreateLimitReached
	case refusal == nil:
		out.Allowed = true
	case refusal.body.Reason == reasonMemberDrivesDisabled:
		out.Reason = client.MemberDriveCreateDisabled
	case refusal.body.Reason == reasonMemberDrivesDeniedByProfile:
		out.Reason = client.MemberDriveCreateDeniedByProfile
		if c, err := s.effectiveCeiling(ctx); err == nil && c.Profile != nil {
			out.DeniedByProfile = c.Profile.Name
		}
	case refusal.body.Reason == reasonMemberDriveUnsupported:
		out.Reason = client.MemberDriveCreateUnsupported
	case refusal.body.Reason == reasonMemberDriveDeleteUnavailable:
		out.Reason = client.MemberDriveCreateDeleteUnavailable
	case refusal.body.Reason == reasonGroupsSnapshotStale:
		out.Reason = client.MemberDriveCreateGroupsStale
	default:
		out.Reason = client.MemberDriveCreateGovernance
	}
	return out
}

// memberDriveOwners is the run's member drives, each id to its owner, for
// dispatch's re-read.
func memberDriveOwners(drives []runDrive) map[uuid.UUID]string {
	var out map[uuid.UUID]string
	for _, d := range drives {
		if owner := d.resolved.Drive.OwnerSubject; owner != "" {
			if out == nil {
				out = map[uuid.UUID]string{}
			}
			out[d.resolved.Drive.ID] = owner
		}
	}
	return out
}

// sandboxCreateAdmitted is dispatch's last doors before CreateSandbox: the
// local placement classification, then the member drives' re-read.
func (s *Server) sandboxCreateAdmitted(ctx context.Context, run types.AgentRun, ceiling dispatchCeiling, sc types.SiteConfig,
	spec *runner.SandboxSpec, orgConfigKeys []string, llm llmTransport, ado adoEntraRun, p dispatchParams) bool {
	return s.classifyLocalDispatch(ctx, run, ceiling, sc, spec, orgConfigKeys, llm, ado, p.TrustedOutput) &&
		s.memberDrivesStillThere(ctx, run, p.MemberDrives)
}

// memberDrivesStillThere re-reads each member drive the run attaches, under its
// owner, right before the sandbox is created. One that is gone or being
// deleted fails the run as drive_not_found, so nothing mounts it and no volume
// or claim is made for it.
func (s *Server) memberDrivesStillThere(ctx context.Context, run types.AgentRun, owners map[uuid.UUID]string) bool {
	if len(owners) == 0 {
		return true
	}
	ms, _ := s.cfg.Store.(interface {
		GetMemberDriveOf(ctx context.Context, owner string, id uuid.UUID) (types.UserDrive, error)
	})
	for id, owner := range owners {
		if ms != nil {
			d, err := ms.GetMemberDriveOf(ctx, owner, id)
			if err == nil && d.DeleteStartedAt == nil {
				continue
			}
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"reason": "drive_not_found"})))
		s.failAndRevoke(ctx, run.ID, types.RunStarting, driveNotFoundMsg(id.String()))
		return false
	}
	return true
}

// templateMemberDrivesRefused refuses a person's own drive in an organisation
// or group template, writing the answer: the people it is offered to cannot
// attach it, and the template would disclose it. The diagnostic names the
// path, never the drive.
func (s *Server) templateMemberDrivesRefused(w http.ResponseWriter, r *http.Request, t types.RunTemplate) bool {
	var doc client.TemplateDocument
	var drives []client.DriveAttachment
	if t.Owner.Scope == types.TemplateScopePerson || s.cfg.Store == nil ||
		json.Unmarshal(t.Document, &doc) != nil || json.Unmarshal(doc.Intent.Raw("drives"), &drives) != nil {
		return false
	}
	var diags []client.TemplateDiagnostic
	for i, a := range drives {
		id, err := uuid.Parse(a.DriveID)
		if err != nil {
			continue
		}
		d, err := s.cfg.Store.GetUserDrive(r.Context(), id)
		switch {
		case errors.Is(err, store.ErrNotFound) || err == nil && d.OwnerSubject == "":
			continue
		case err != nil:
			writeServerError(w, r, "read a template's drive", err)
			return true
		}
		path := fmt.Sprintf("intent.drives[%d]", i)
		diags = append(diags, client.TemplateDiagnostic{Path: path, Reason: reasonTemplateSharedFieldRefused, Message: templateSharedFieldRefusedMsg(path, string(t.Owner.Scope))})
	}
	if len(diags) == 0 {
		return false
	}
	writeTemplateDiagnostics(w, diags...)
	return true
}
