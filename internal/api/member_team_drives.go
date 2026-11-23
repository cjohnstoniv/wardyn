// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// A person's own team drive (#3459): a member drive made with team, which belongs to the governance unit its maker
// is in and is shared with everyone in that unit or a unit below it, as one folder. Reach follows the pool use
// rule for units (types.RunnerPool): the caller's unit and every unit above it, or a queued start's frozen unit.
// The maker stays the owner, so delete, its fence and the count cap are the member drive ones.
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func memberTeamDrivesDisabledMsg() string {
	return "Your organisation doesn't let people make team drives. Ask an admin."
}

func memberTeamDriveUnsupportedMsg() string {
	return "A team drive you make is a Docker volume, so this server can't make one."
}

func memberTeamDriveMacMsg() string {
	return "A team drive is a Docker volume, so it can't be a disk kept on a Mac. Leave out runner_id."
}

func memberTeamDriveNoUnitMsg() string {
	return "A team drive is shared with a governance unit other than Default, and you aren't in one. Ask an admin to add you to a unit."
}

// memberTeamDriveGate is POST /me/drives' door for team: the unit the drive belongs to, or the refusal. nil, nil
// when team is not asked. Both switches must be on, the deployment Docker, the request no Mac, and the caller in a
// unit that is not Default (a team drive there would be shared with the whole organisation).
func (s *Server) memberTeamDriveGate(r *http.Request, policy types.MemberDrivePolicy, team, mac bool) (*uuid.UUID, *runRefusal) {
	if !team {
		return nil, nil
	}
	if mac {
		return nil, runError(http.StatusUnprocessableEntity, reasonMemberTeamDriveUnsupported, memberTeamDriveMacMsg())
	}
	ctx := r.Context()
	provider, err := s.userDriveProvider(ctx)
	if err != nil {
		return nil, runServerError("get site config", err)
	}
	if !provider.TeamDrives || !policy.TeamDrives {
		return nil, runError(http.StatusUnprocessableEntity, reasonMemberTeamDrivesDisabled, memberTeamDrivesDisabledMsg())
	}
	if s.cfg.RunnerTarget != "docker" {
		return nil, runError(http.StatusUnprocessableEntity, reasonMemberTeamDriveUnsupported, memberTeamDriveUnsupportedMsg())
	}
	ceiling, err := s.effectiveCeiling(ctx)
	if err != nil {
		return nil, ceilingRunRefusal(err)
	}
	if ceiling.Unit == nil {
		return nil, runError(http.StatusUnprocessableEntity, reasonMemberTeamDriveNoUnit, memberTeamDriveNoUnitMsg())
	}
	units, err := s.unitOutlines(ctx)
	if err != nil {
		return nil, runServerError("read governance units", err)
	}
	i := slices.IndexFunc(units, func(u types.GovernanceUnit) bool { return u.ID == ceiling.Unit.ID })
	if i < 0 || isDefaultUnit(units[i]) {
		return nil, runError(http.StatusUnprocessableEntity, reasonMemberTeamDriveNoUnit, memberTeamDriveNoUnitMsg())
	}
	return &ceiling.Unit.ID, nil
}

// memberTeamDrivesOffered is the catalogue's team_drives_allowed: the create gate asked for a team drive on no Mac.
// A read that fails is false.
func (s *Server) memberTeamDrivesOffered(r *http.Request, policy types.MemberDrivePolicy) bool {
	_, refusal := s.memberTeamDriveGate(r, policy, true, false)
	return refusal == nil
}

// insertMemberOrTeamDrive creates d as a member drive, or with unit as that unit's team drive.
func (s *Server) insertMemberOrTeamDrive(ctx context.Context, ms store.MemberDriveStore, d types.UserDrive, limit int,
	unit *uuid.UUID) (types.UserDrive, error) {
	if unit == nil {
		return ms.CreateMemberDrive(ctx, d, limit)
	}
	ts, ok := ms.(store.MemberTeamDriveStore)
	if !ok {
		return types.UserDrive{}, errors.New("api: the store keeps no team drives")
	}
	return ts.CreateMemberTeamDrive(ctx, d, *unit, limit)
}

// memberTeamReachableDrives appends the team drives people made that the caller reaches: those of the caller's unit
// or a unit above it (or a queued start's frozen unit), and the caller's own. Each resolves through an in-memory
// unit-tier allocation, so it mounts as the one shared folder, read-only for everyone but its maker until a super
// admin ticks members can write. With storage.user_drive.team_drives off only the maker sees it, as unmountable.
// The caller rules are memberReachableDrives'.
func (s *Server) memberTeamReachableDrives(ctx context.Context, out []reachableDrive, ceiling driveSizeCeiling) ([]reachableDrive, error) {
	ts, ok := s.cfg.Store.(store.MemberTeamDriveStore)
	owner := types.CanonicalUserSubject(oidcHumanFromContext(ctx))
	if _, actor := actorFromContext(ctx, ""); !ok || owner == "" || types.CanonicalUserSubject(actor) != owner {
		return out, nil
	}
	var units []uuid.UUID
	for _, u := range s.poolUnits(ctx) {
		if id, err := uuid.Parse(u); err == nil {
			units = append(units, id)
		}
	}
	rows, err := ts.ListReachableMemberTeamDrives(ctx, owner, units)
	if err != nil {
		return nil, fmt.Errorf("api: list team drives: %w", err)
	}
	for _, row := range rows {
		owned := row.Drive.OwnerSubject == owner
		subject := owner
		if row.UnitID != nil {
			subject = row.UnitID.String()
		}
		r := reachableDrive{owned: owned, legacyHeld: row.Drive.LegacyUnsettled, winner: store.UserDriveWinner{
			Drive: row.Drive, Grant: types.UserDriveGrant{
				SubjectType: types.DriveSubjectUnit, Subject: subject, DriveID: row.Drive.ID, Enabled: true,
				MembersCanWrite: owned || row.MembersCanWrite,
			}}}
		if !ceiling.TeamDrives {
			if !owned {
				continue
			}
			r.err = fmt.Errorf("%w: %s", errDriveUnmountable, driveTeamOffMsg)
			out = append(out, r)
			continue
		}
		r.resolved, r.err = newResolvedDrive(&r.winner.Drive, &r.winner.Grant, types.DriveSubjectUnit, []string{owner}, ceiling)
		if r.err != nil && !errors.Is(r.err, errDriveUnmountable) {
			return nil, r.err
		}
		out = append(out, r)
	}
	return out, nil
}

// memberTeamDriveSwitchOn is the platform switch at dispatch's re-read of a run owner's own member drive: false for
// a team drive while storage.user_drive.team_drives is off or unreadable, so not even its owner's run mounts it; true
// for any other member drive.
func (s *Server) memberTeamDriveSwitchOn(ctx context.Context, d types.UserDrive) bool {
	if !d.Team {
		return true
	}
	provider, err := s.userDriveProvider(ctx)
	return err == nil && provider.TeamDrives
}

// memberTeamSharedWithUnit is whether a catalogue row is a person's team drive that its unit reaches: false for one
// whose unit was deleted, which memberTeamReachableDrives allocates to its owner alone.
func memberTeamSharedWithUnit(w store.UserDriveWinner) bool {
	return w.Grant.SubjectType == types.DriveSubjectUnit && w.Grant.Subject != w.Drive.OwnerSubject
}

// memberTeamDriveStillThere is dispatch's re-read of another person's team drive under the run's frozen unit: true
// only while it is a live team drive, not being deleted and not legacy-held, team drives are on, and its unit is the
// run's unit or one above it.
func (s *Server) memberTeamDriveStillThere(ctx context.Context, run types.AgentRun, id uuid.UUID) bool {
	ts, ok := s.cfg.Store.(store.MemberTeamDriveStore)
	if !ok {
		return false
	}
	t, err := ts.GetMemberTeamDrive(ctx, id)
	if err != nil || t.Drive.Deleting() || t.Drive.LegacyUnsettled || t.UnitID == nil {
		return false
	}
	if provider, err := s.userDriveProvider(ctx); err != nil || !provider.TeamDrives {
		return false
	}
	return slices.Contains(s.unitLineage(ctx, run.UnitID), t.UnitID.String())
}

// memberDriveHome is the home a member drive's object is named from: a team drive's one shared folder, else the
// owner's hash home.
func memberDriveHome(d types.UserDrive) (string, error) {
	if d.Team {
		return types.DriveTeamHome(d), nil
	}
	return types.DriveHomeName(d, d.OwnerSubject, "")
}

// memberDriveSubjectHash is the person a member drive's object is labelled with: none for a team drive, whose
// folder belongs to no one person (teamResolvedDrive mounts it so), else the owner.
func memberDriveSubjectHash(d types.UserDrive) string {
	if d.Team {
		return ""
	}
	return types.DriveSubjectHash(d.OwnerSubject)
}

// memberDriveStarting is whether a run that may re-read d is starting. A team drive is re-read by every unit
// member's run, so its delete waits for every starting run, not only its owner's.
func (s *Server) memberDriveStarting(ctx context.Context, ms store.MemberDriveStore, d types.UserDrive) (bool, error) {
	if !d.Team {
		return ms.MemberDriveOwnerStarting(ctx, d.OwnerSubject, watcherLeaseStaleAfter)
	}
	ts, ok := ms.(store.MemberTeamDriveStore)
	if !ok {
		return true, nil
	}
	return ts.AnyRunStarting(ctx, watcherLeaseStaleAfter)
}

// memberDriveAdminItem is one person's drive as GET /drives/member lists it.
func memberDriveAdminItem(d types.UserDrive) client.MemberDriveAdminItem {
	mount, _ := memberDriveMount(d)
	return client.MemberDriveAdminItem{
		ID: d.ID, Name: d.Name, Owner: d.OwnerSubject, Backend: d.Backend, HomeRunnerID: d.HomeRunnerID, SizeMiB: d.SizeMiB,
		StorageClass: d.StorageClass, Object: mount.ObjectName, CreatedAt: d.CreatedAt, Deleting: d.Deleting(), Team: d.Team,
	}
}

// memberTeamDriveFactsOf reads the team facts of the team drives among drives, and the names of their units. A
// failed read leaves them out: the list is a display read.
func (s *Server) memberTeamDriveFactsOf(ctx context.Context, drives []types.UserDrive) (map[uuid.UUID]store.MemberTeamDrive, map[uuid.UUID]string) {
	ts, ok := s.cfg.Store.(store.MemberTeamDriveStore)
	var ids []uuid.UUID
	for _, d := range drives {
		if d.Team {
			ids = append(ids, d.ID)
		}
	}
	if !ok || len(ids) == 0 {
		return nil, nil
	}
	facts, _ := ts.MemberTeamDriveFacts(ctx, ids)
	return facts, s.memberTeamDriveUnitNames(ctx, facts)
}

// memberTeamDriveUnitNames names the units of facts, read once; nil when none has a unit or the read failed.
func (s *Server) memberTeamDriveUnitNames(ctx context.Context, facts map[uuid.UUID]store.MemberTeamDrive) map[uuid.UUID]string {
	hasUnit := false
	for _, t := range facts {
		hasUnit = hasUnit || t.UnitID != nil
	}
	if !hasUnit {
		return nil
	}
	units, err := s.unitOutlines(ctx)
	if err != nil {
		return nil
	}
	names := make(map[uuid.UUID]string, len(units))
	for _, u := range units {
		names[u.ID] = u.Name
	}
	return names
}

// memberTeamDriveAdminFields fills a list item's team facts from t, so the list and the write bit's answer agree.
func memberTeamDriveAdminFields(item *client.MemberDriveAdminItem, t store.MemberTeamDrive, names map[uuid.UUID]string) {
	item.Team, item.TeamUnitID, item.MembersCanWrite = true, t.UnitID, t.MembersCanWrite
	if t.UnitID != nil {
		item.TeamUnitName = names[*t.UnitID]
	}
}

// handlePutMemberTeamDrive is PUT /drives/member/{id}/team: a super admin lets a person's team drive's unit write to
// it, or stops it. 200 with the drive's row; 404 for an id that is not a person's team drive; 409 while it is being
// deleted. operatorOnly.
func (s *Server) handlePutMemberTeamDrive(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "member drive")
	if !ok {
		return
	}
	var req client.MemberTeamDriveWrite
	if !decodeStrict(w, r, &req) {
		return
	}
	ts, ok := s.cfg.Store.(store.MemberTeamDriveStore)
	if !ok {
		writeErrorReason(w, http.StatusNotFound, reasonUserDriveNotFound, "member drive not found")
		return
	}
	before, after, err := ts.SetMemberTeamDriveWrite(r.Context(), id, req.MembersCanWrite)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonUserDriveNotFound, "member team drive not found")
		return
	case errors.Is(err, store.ErrMemberDriveBusy):
		writeErrorReason(w, http.StatusConflict, reasonMemberDriveBusy, memberDriveBusyMsg())
		return
	case err != nil:
		writeServerError(w, r, "set member team drive", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"drive.member_team.change", id.String(), "success", mustJSON(map[string]any{
			"drive": after.Drive.Name, "drive_id": id, "owner": after.Drive.OwnerSubject, "unit_id": after.UnitID,
			"members_can_write": after.MembersCanWrite, "was": before.MembersCanWrite,
		})))
	item := memberDriveAdminItem(after.Drive)
	memberTeamDriveAdminFields(&item, after, s.memberTeamDriveUnitNames(r.Context(), map[uuid.UUID]store.MemberTeamDrive{id: after}))
	writeJSON(w, http.StatusOK, item)
}
