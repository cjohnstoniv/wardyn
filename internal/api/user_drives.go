// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// User-drive CRUD (0.7, migration 0054): the admin-facing surface over
// user_drives + user_drive_grants — the storage an admin registers and the rows
// allocating one to a user, a group, or everyone.
//
// Modeled on governance.go, deliberately and almost line for line: one read
// that shows the whole picture, small validated writes, an audit event per
// write, and the ON DELETE RESTRICT surfaced as a 409. The two tables answer
// the same shape of question about the same subject vocabulary, so a reader who
// has understood one has understood this one — and the two cannot drift on the
// thing that matters, which is what "who" means.
//
// TWO THINGS ARE SPECIFIC TO THIS TABLE, and neither is optional:
//
//  1. THE ENV CEILING. A host_path drive's host_root is authored HERE, in the
//     database, and then bound into OTHER PEOPLE's sandboxes. So the write
//     boundary composes two checks: types.ValidateUserDrive for the row's shape
//     and runner.UserDriveHostRootCheck for the deployment's operator/MDM-set
//     ceiling over it. A deployment that sets no roots can author no host_path
//     drive at all — an admin cannot widen the ceiling from inside the product.
//
//  2. A NEW GRANT IS ENABLED. The store writes UserDriveGrant.Enabled verbatim,
//     so a zero-valued grant is a PAUSED one. `enabled` therefore decodes as a
//     *bool defaulting true here, at the request boundary, which is the only
//     place that can tell "the client said false" from "the client said
//     nothing" — see userDriveGrantRequest.Enabled.
//
// EVERY ROUTE IS SUPER-ONLY, not securityOps; the tier argument is written at
// the mountUserDriveRoutes call that decides it (routes.go).
package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mountUserDriveRoutes registers the /drives family — SEVEN routes, all on the
// SUPER-admin tier.
//
// The parameter is spelled `operatorOnly` because that is the group routes.go
// hands it and the tier these were born on; the group a mount function receives
// is decided AT THE CALL SITE, never by this parameter's name, and
// authz_test.go's chi.Walk matrix is what enforces the classification.
//
// Registered UNCONDITIONALLY, with no `if s.cfg.Store != nil` arm: a route that
// appears only on some deployments is a route the authorization matrix has to
// arrange for, and the every-conditional-route-mounted doctrine exists so it
// never has to.
func (s *Server) mountUserDriveRoutes(operatorOnly chi.Router) {
	operatorOnly.Get("/drives", s.handleGetUserDrives)
	operatorOnly.Post("/drives", s.handleCreateUserDrive)
	operatorOnly.Put("/drives/{id}", s.handleUpdateUserDrive)
	operatorOnly.Delete("/drives/{id}", s.handleDeleteUserDrive)
	operatorOnly.Post("/drives/grants", s.handleUpsertUserDriveGrant)
	operatorOnly.Delete("/drives/grants/{id}", s.handleDeleteUserDriveGrant)
	operatorOnly.Post("/drives/preview", s.handlePreviewUserDrive)
}

// ─── GET /drives ───────────────────────────────────────────────────────────

// userDrivesResponse is GET /drives's body: the whole Drives screen in ONE
// call, the same "one read shows the picture" shape GET /governance takes.
//
// The two lists are separate rather than nested for the reason governance's
// are: a grant's whole meaning is WHICH drive it points at, and nesting drives
// under grants would duplicate a drive per allocation while hiding an
// un-allocated one entirely — which is exactly the state ("allocated to
// nobody") the admin surface has to be able to show.
//
// The two scalars are the DEPLOYMENT facts the console cannot derive and would
// otherwise guess at:
//
//   - HostRootsConfigured says whether WARDYN_USER_DRIVE_HOST_ROOTS is set at
//     all, so the backend picker can disable `host_path` WITH THE REASON rather
//     than offering an option whose every save 422s. It is a BOOLEAN, never the
//     roots themselves: the console needs to know that a ceiling exists, not
//     where the operator's filesystem is laid out.
//   - RunnerTarget is which substrate this deployment dispatches to, so the
//     picker can offer the two backends that can actually mount here instead of
//     letting an admin author a k8s drive on a Docker install and learn about it
//     from a 400.
type userDrivesResponse struct {
	Drives              []types.UserDriveListItem `json:"drives"`
	Grants              []types.UserDriveGrant    `json:"grants"`
	HostRootsConfigured bool                      `json:"host_roots_configured"`
	RunnerTarget        string                    `json:"runner_target"`
}

// handleGetUserDrives returns the whole drives picture. operatorOnly
// (routes.go).
func (s *Server) handleGetUserDrives(w http.ResponseWriter, r *http.Request) {
	drives, err := s.cfg.Store.ListUserDrives(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list user drives: "+err.Error())
		return
	}
	grants, err := s.cfg.Store.ListUserDriveGrants(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list user drive grants: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, userDrivesResponse{
		Drives:              drives,
		Grants:              grants,
		HostRootsConfigured: len(s.cfg.UserDriveHostRoots) > 0,
		RunnerTarget:        s.cfg.RunnerTarget,
	})
}

// ─── drive writes ──────────────────────────────────────────────────────────

// userDriveRequest is the POST/PUT body. ID/CreatedAt/UpdatedAt/CreatedBy are
// never accepted from the wire: the id comes from the path (PUT) or the server
// (POST), and provenance is always server-assigned — the same rule
// governanceProfileRequest states.
type userDriveRequest struct {
	Name         string             `json:"name"`
	Backend      types.DriveBackend `json:"backend"`
	HostRoot     string             `json:"host_root,omitempty"`
	StorageClass string             `json:"storage_class,omitempty"`
	HomeTemplate types.HomeTemplate `json:"home_template,omitempty"`
	SizeMiB      int                `json:"size_mib,omitempty"`
	Writable     bool               `json:"writable,omitempty"`
	Reclaim      types.DriveReclaim `json:"reclaim,omitempty"`
}

// userDriveHostRootCheck is the deployment's ceiling over an admin-authored
// host_path, built from the boot-parsed roots. It returns the hook type
// internal/types names but cannot implement, which is the whole point of the
// hook: internal/types must not read the environment, and a ceiling that lived
// in two places would be a ceiling one of them could forget.
//
// EMPTY ROOTS REFUSE EVERY host_path DRIVE, and the refusal is inside the
// closure rather than a caller's `if`, so no call site can acquire the
// fail-open version by forgetting the guard.
func (s *Server) userDriveHostRootCheck() types.UserDriveHostRootCheck {
	return runner.UserDriveHostRootCheck(s.cfg.UserDriveHostRoots)
}

// decodeUserDriveRequest decodes, normalizes and validates a drive write body,
// returning the HTTP status and message the caller answers with (0, "" on
// success) and the validated row.
//
// FOUR GATES IN ORDER, and the order is the argument:
//
//  1. Strict decoding (decodeStrictMsg, which is also where the 1 MiB body cap
//     rides — an unknown field is a typo that must not silently widen
//     behaviour, the LoadPolicySpec discipline). readCappedBody is deliberately
//     NOT used: it hands back raw bytes, and unmarshalling those would lose
//     DisallowUnknownFields, which is the half of the cap that matters here.
//  2. types.ValidateUserDrive — the row's SHAPE, including the backend-vs-
//     RunnerTarget match, which is a 400 because it is a statement about the
//     request that no deployment state can make true.
//  3. The ENV CEILING over host_root — a 422, because the request is
//     well-formed and it is the DEPLOYMENT that cannot accept it. The
//     distinction is the one denyMemberRunQuota draws: a 400 says "you wrote
//     this wrong", a 422 says "there is nothing here to write it into".
//  4. NESTING against the other stored host_path roots (driveHostRootNesting)
//     — a 422 for the same reason as 3, and LAST because it is the only gate
//     that reads the database: a row that is malformed or outside the ceiling
//     must be refused without a store round-trip.
func (s *Server) decodeUserDriveRequest(w http.ResponseWriter, r *http.Request, id uuid.UUID) (types.UserDrive, int, string) {
	var req userDriveRequest
	if msg := decodeStrictMsg(w, r, &req); msg != "" {
		return types.UserDrive{}, http.StatusBadRequest, msg
	}
	d := types.UserDrive{
		ID:           id,
		Name:         req.Name,
		Backend:      req.Backend,
		HostRoot:     req.HostRoot,
		StorageClass: req.StorageClass,
		HomeTemplate: req.HomeTemplate,
		SizeMiB:      req.SizeMiB,
		Writable:     req.Writable,
		Reclaim:      req.Reclaim,
		CreatedBy:    principalFromRequest(r),
	}
	if err := types.ValidateUserDrive(&d, s.cfg.RunnerTarget); err != nil {
		return types.UserDrive{}, http.StatusBadRequest, "invalid drive: " + err.Error()
	}
	if d.Backend == types.DriveBackendHostPath {
		if err := s.userDriveHostRootCheck()(d.HostRoot); err != nil {
			return types.UserDrive{}, http.StatusUnprocessableEntity, "invalid drive: " + err.Error()
		}
		if code, msg := s.driveHostRootNesting(r, d); msg != "" {
			return types.UserDrive{}, code, msg
		}
	}
	return d, 0, ""
}

// driveHostRootNesting is the FOURTH gate, and the only one that has to look at
// the other ROWS: a host_path drive whose host_root sits inside — or contains —
// another host_path drive's host_root is refused, 422, naming the other drive.
//
// THE HOLE IT CLOSES IS A MEMBER'S, NOT AN ADMIN'S TYPO. Drive A is rooted at
// /srv/shares and gives alice a WRITABLE home at /srv/shares/alice. Drive B is
// then rooted at /srv/shares/alice/team. Nothing above notices: B's root is
// inside the deployment's ceiling, exists, is not a credential directory and is
// an ordinary path. But its whole tree is a directory alice can write from
// INSIDE a run — so she can replace `team`, or any segment under it, with a
// link, and B's members are bound wherever she points them. The driver's
// resolved-real-path checks bound where that can aim (the ceiling, and now the
// member's own home name) but they cannot make the layout supportable: two
// drives sharing a tree means one drive's members author the other drive's
// storage.
//
// STRICT nesting only. Two drives on the SAME root are left alone: that is the
// ordinary "one share, two allocations with different home templates" shape, and
// neither drive's members can move the other's root, because the root is not
// inside anybody's home. Equal roots are a naming question; nested roots are a
// containment one.
//
// LEXICAL, on the already-cleaned stored strings. The symlink half belongs to
// UserDriveHostRootCheck, which resolved both roots against the deployment's
// ceiling when each was written and does it again at bind time; what THIS gate
// owns is the relationship between two rows, which is exactly what the rows say.
//
// ONE STORE READ, on the drive-write path only — a handful of calls in a
// deployment's lifetime, and the same list the console already loads on every
// visit to the screen.
func (s *Server) driveHostRootNesting(r *http.Request, d types.UserDrive) (int, string) {
	drives, err := s.cfg.Store.ListUserDrives(r.Context())
	if err != nil {
		// 500, never "no other drives": a list that failed cannot say the tree is
		// clear, and treating it as clear is how the check silently stops biting
		// on exactly the deployment whose database is unhappy.
		return http.StatusInternalServerError, "list user drives: " + err.Error()
	}
	for _, other := range drives {
		// A row is not its own ancestor: a PUT that re-saves a drive unchanged
		// must not start refusing itself.
		if other.ID == d.ID || other.Backend != types.DriveBackendHostPath || other.HostRoot == "" {
			continue
		}
		switch {
		case strings.HasPrefix(d.HostRoot, other.HostRoot+"/"):
			return http.StatusUnprocessableEntity, fmt.Sprintf(
				"invalid drive: host_root %q is inside drive %q's host_root %q — that tree holds directories the other drive's "+
					"members can write from inside a run, so they could redirect this one; give the two drives separate trees",
				d.HostRoot, other.Name, other.HostRoot)
		case strings.HasPrefix(other.HostRoot, d.HostRoot+"/"):
			return http.StatusUnprocessableEntity, fmt.Sprintf(
				"invalid drive: host_root %q contains drive %q's host_root %q — this drive's members could redirect that one "+
					"from inside a run; give the two drives separate trees",
				d.HostRoot, other.Name, other.HostRoot)
		}
	}
	return 0, ""
}

// writeUserDrive is the shared body of POST and PUT: decode, validate against
// both the shape rules and the env ceiling, refuse a silent re-homing, persist,
// audit, answer.
//
// The re-home guard runs LAST of the gates and needs no "is this a PUT" flag:
// it keys on whether the id already names a row with allocations, and
// handleCreateUserDrive mints a fresh uuid, so a create finds nothing and the
// guard is a single list read that returns immediately.
func (s *Server) writeUserDrive(w http.ResponseWriter, r *http.Request, id uuid.UUID, status int) {
	d, code, msg := s.decodeUserDriveRequest(w, r, id)
	if msg != "" {
		writeError(w, code, msg)
		return
	}
	if code, msg := s.driveRehomeGuard(r, d); msg != "" {
		writeError(w, code, msg)
		return
	}
	saved, err := s.cfg.Store.UpsertUserDrive(r.Context(), d)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, fmt.Sprintf("a user drive named %q already exists", d.Name))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "write user drive: "+err.Error())
		return
	}
	// The WHOLE ROW, minus nothing: a drive carries no secret (a share
	// credential is the operator's, held host-side and never by Wardyn — see
	// types.DriveBackendHostPath), and host_root is the single most audit-worthy
	// field on it, since it is the host tree this row just authorized binding
	// into other people's sandboxes.
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"drive.write", saved.ID.String(), "success", mustJSON(map[string]any{
			"name":          saved.Name,
			"backend":       saved.Backend,
			"host_root":     saved.HostRoot,
			"storage_class": saved.StorageClass,
			"home_template": saved.HomeTemplate,
			"size_mib":      saved.SizeMiB,
			"writable":      saved.Writable,
			"reclaim":       saved.Reclaim,
		})))
	writeJSON(w, status, saved)
}

// handleCreateUserDrive mints a fresh id and writes a new drive (201). A name
// another drive already holds is a caller-fixable 409, never a raw driver
// error. operatorOnly (routes.go).
func (s *Server) handleCreateUserDrive(w http.ResponseWriter, r *http.Request) {
	s.writeUserDrive(w, r, uuid.New(), http.StatusCreated)
}

// handleUpdateUserDrive replaces the drive at {id} (200), rename included —
// renaming has to work, because ON DELETE RESTRICT makes delete-and-recreate
// impossible for a drive that is actually allocated. A PUT naming an id no row
// holds creates it there, which is what PUT means and what UpsertUserDrive's
// single statement does without an existence read. operatorOnly (routes.go).
//
// An IDENTITY-AFFECTING change on an ALLOCATED drive answers 409 unless the
// request says it means it — see driveRehomeGuard, which holds the argument.
func (s *Server) handleUpdateUserDrive(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "user drive")
	if !ok {
		return
	}
	s.writeUserDrive(w, r, id, http.StatusOK)
}

// driveIdentityFields lists the identity-affecting differences between a stored
// drive and the one a PUT would replace it with.
//
// The four columns are the ones every allocated person's STORAGE OBJECT is
// derived from (types.DriveObjectName over the drive and the home its template
// yields). Changing one does not edit a drive: it points every allocation at a
// DIFFERENT object, all at once, and leaves the old ones behind with nothing in
// the product naming them.
//
//   - backend — a host_path → docker_volume flip moves everyone off their NAS
//     home onto a fresh, empty volume.
//   - home_template — sub → hash re-derives every home; the old objects survive
//     and are findable only by the wardyn.drive label.
//   - host_root — the same home names, bound against a different tree, from the
//     next run onward.
//   - name — a k8s object is `wardyn-drive-<drive-slug>-<home>`, so a rename
//     re-homes every claim on a Kubernetes deployment. It is inert on Docker and
//     is listed anyway: which substrate a stored row is on is not something this
//     gate should have to be right about to stay safe.
//
// Stable order, so the same edit reads the same way twice.
func driveIdentityFields(before, after types.UserDrive) []string {
	var out []string
	for _, f := range []struct{ name, old, updated string }{
		{"backend", string(before.Backend), string(after.Backend)},
		{"home_template", string(before.HomeTemplate), string(after.HomeTemplate)},
		{"host_root", before.HostRoot, after.HostRoot},
		{"name", before.Name, after.Name},
	} {
		if f.old != f.updated {
			out = append(out, fmt.Sprintf("%s %q → %q", f.name, f.old, f.updated))
		}
	}
	return out
}

// driveRehomeConfirm is the query value that means "yes, re-home them":
// `PUT /drives/{id}?confirm=rehome`.
//
// A QUERY PARAMETER RATHER THAN A BODY FIELD, and the choice is not cosmetic.
// A PUT's body is the drive ROW, and decodeStrict refuses unknown fields in it
// precisely so a typo cannot be stored — while a confirmation is not a column,
// it is a property of this one request. In the body it would be carried back by
// every client that GETs a drive and PUTs it again, which is the one shape a
// confirmation must never have; and it would force DisallowUnknownFields to
// tolerate a key that is not part of the resource, weakening the write boundary
// for the sake of a flag.
const driveRehomeConfirm = "rehome"

// driveRehomeGuard refuses an identity-affecting PUT on a drive that already has
// ALLOCATIONS, unless the request carries ?confirm=rehome. It returns (0, "")
// when the write may proceed.
//
// WHY A DOOR AND NOT A WARNING. UpsertUserDrive writes every column in place, so
// before this gate a PUT changing `home_template` on a drive with forty grants
// answered 200 and re-homed forty people silently: their next run mounts a
// different object, the one holding their work is orphaned, and nothing in the
// product points at it any more. That is the class of act handleDeleteUserDrive
// already refuses to perform quietly (ON DELETE RESTRICT), and it earns the same
// status — a 409 is "the state of this resource makes that unsafe", which is
// exactly the claim.
//
// WHY A CONFIRMATION AND NOT A REFUSAL. Re-homing is sometimes precisely what an
// admin means — a share re-mounted at a new root, a directory convention that
// changed — and delete-and-recreate is not open to them while the FK RESTRICTs.
// So the gate names both halves an admin needs in order to decide: WHAT changes,
// and HOW MANY allocations it moves.
//
// AN UNALLOCATED DRIVE MEETS NOTHING. With no grants nothing is re-homed, which
// is the state these fields are corrected in most often — a row authored a
// minute ago.
//
// ONE STORE READ, on the drive-write path only, and it is ListUserDrives because
// that read carries the existing row AND its grant count together; a GetUserDrive
// plus a grant scan would be two reads for one question. A read that FAILS is a
// 500 and never "no grants": a list that could not answer cannot say a drive is
// unallocated, and treating it as unallocated is how a gate silently stops
// biting on exactly the deployment whose database is unhappy — the ordering rule
// driveHostRootNesting states for the same reason.
func (s *Server) driveRehomeGuard(r *http.Request, d types.UserDrive) (int, string) {
	drives, err := s.cfg.Store.ListUserDrives(r.Context())
	if err != nil {
		return http.StatusInternalServerError, "list user drives: " + err.Error()
	}
	var before *types.UserDriveListItem
	for i := range drives {
		if drives[i].ID == d.ID {
			before = &drives[i]
			break
		}
	}
	// A PUT that CREATES (an id no row holds) re-homes nobody, and neither does
	// one on a drive nothing is allocated from.
	if before == nil || before.GrantCount == 0 {
		return 0, ""
	}
	changes := driveIdentityFields(before.UserDrive, d)
	if len(changes) == 0 || r.URL.Query().Get("confirm") == driveRehomeConfirm {
		return 0, ""
	}
	them := "it"
	if before.GrantCount > 1 {
		them = "them"
	}
	return http.StatusConflict, fmt.Sprintf(
		"this drive is allocated to %s and this change re-homes %s: %s. Every allocated person's storage object is derived from "+
			"these fields, so their next run mounts a different object and the one holding their work is left behind with nothing "+
			"in Wardyn naming it. Re-send with ?confirm=%s if that is what you mean.",
		pluralDriveSubjects(before.GrantCount), them, strings.Join(changes, ", "), driveRehomeConfirm)
}

// pluralDriveSubjects counts a drive's allocations the way the console's
// ALLOCATED_COUNT does — "1 subject" / "n subjects" — so the wire and the screen
// count the same things in the same words. ALLOCATIONS, never people: a group
// allocation is one row and Wardyn holds no directory read, so "14 people" would
// be a claim rather than a count.
func pluralDriveSubjects(n int) string {
	if n == 1 {
		return "1 subject"
	}
	return fmt.Sprintf("%d subjects", n)
}

// handleDeleteUserDrive removes a drive (204), or 409 when it is still
// ALLOCATED.
//
// The 409 is the whole point of the FK's ON DELETE RESTRICT: cascading the
// grants away would silently un-allocate every person this drive named a
// directory for, leaving those directories on the share or the volume with
// nothing in the product pointing at them. Refusing makes the un-allocation a
// deliberate, separately-audited act (delete the grants first), which is also
// the offboarding step an operator's runbook wants recorded.
//
// THE MESSAGE CARRIES NO COUNT, and must not grow one (the mock round's own
// ruling): the console's DELETE_RESTRICT_BODY is the client-side pre-fill and
// names the count the list it is looking at already shows. This body is what the
// RACE path renders — the client believed the count was zero — and a count read
// here would be a second, later number contradicting the one on screen, from a
// read the refusal does not need. operatorOnly (routes.go).
func (s *Server) handleDeleteUserDrive(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "user drive")
	if !ok {
		return
	}
	err := s.cfg.Store.DeleteUserDrive(r.Context(), id)
	if notFoundIf(w, err, "user drive") {
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "this drive is still allocated — remove its allocations first "+
			"(deleting it while allocated would leave those subjects with a mount that names nothing)")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete user drive: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"drive.delete", id.String(), "success", nil))
	w.WriteHeader(http.StatusNoContent)
}

// ─── grant writes ──────────────────────────────────────────────────────────

// userDriveGrantRequest is POST /drives/grants's body. The natural key
// (subject_type, subject) is what a caller names; the row's id and provenance
// are server-assigned.
type userDriveGrantRequest struct {
	SubjectType     types.CapabilitySubjectType `json:"subject_type"`
	Subject         string                      `json:"subject"`
	DriveID         uuid.UUID                   `json:"drive_id"`
	Priority        int                         `json:"priority"`
	SizeMiBOverride int                         `json:"size_mib_override,omitempty"`
	// WritableOverride is TRI-STATE on the wire exactly as it is in the column:
	// absent inherits the drive's posture, and an explicit false is an admin
	// saying "this subject reads only" on a writable drive.
	WritableOverride *bool  `json:"writable_override,omitempty"`
	HomeOverride     string `json:"home_override,omitempty"`
	// Enabled is a *bool DEFAULTING TRUE, and this is the one field on this
	// surface whose zero value would be a security-shaped bug rather than a
	// cosmetic one. UpsertUserDriveGrant writes Enabled VERBATIM, so a plain
	// bool would make every grant written by a client that omits the field —
	// every CLI script, every curl, the console before it learned the field —
	// arrive PAUSED. An admin would allocate a drive, see it listed, and the
	// member would get nothing, with no error anywhere to explain it.
	//
	// The pointer is what lets this boundary tell "the client said false" (pause
	// it) from "the client said nothing" (the obvious meaning of allocating a
	// drive: give it to them). Absent-means-on is safe here precisely because
	// the grant itself is the admin's deliberate act — the default only decides
	// whether that act takes effect now or never.
	Enabled *bool `json:"enabled,omitempty"`
}

// handleUpsertUserDriveGrant allocates one drive to one subject, keyed on the
// natural (subject_type, subject): re-allocating a subject REPOINTS its single
// row rather than accumulating a second — which is also what makes "one drive
// per principal" true in the schema and not only in the resolver's LIMIT 1.
//
// 201 for a genuinely new allocation, 200 when an existing one was repointed —
// the same created/updated signal handleUpsertGovernanceAssignment gives,
// derived the same way (the store returns the EXISTING row's id on a conflict,
// never the candidate's). operatorOnly (routes.go).
func (s *Server) handleUpsertUserDriveGrant(w http.ResponseWriter, r *http.Request) {
	var req userDriveGrantRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	g := types.UserDriveGrant{
		SubjectType:      req.SubjectType,
		Subject:          req.Subject,
		DriveID:          req.DriveID,
		Priority:         req.Priority,
		SizeMiBOverride:  req.SizeMiBOverride,
		WritableOverride: req.WritableOverride,
		HomeOverride:     req.HomeOverride,
		Enabled:          req.Enabled == nil || *req.Enabled,
	}
	// Shape first (types.ValidateUserDriveGrant owns the subject hygiene and the
	// user-tier-only home_override rule), then the row is what the store sees.
	if err := types.ValidateUserDriveGrant(&g); err != nil {
		writeError(w, http.StatusBadRequest, "invalid allocation: "+err.Error())
		return
	}
	g.ID = uuid.New()
	g.CreatedBy = principalFromRequest(r)
	saved, err := s.cfg.Store.UpsertUserDriveGrant(r.Context(), g)
	// ErrNotFound here is the FK refusing an unknown drive_id — a 404 naming the
	// drive, not a 500, and not a silent no-op.
	if notFoundIf(w, err, "user drive") {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "upsert user drive grant: "+err.Error())
		return
	}
	status := http.StatusCreated
	if saved.ID != g.ID {
		status = http.StatusOK
	}
	// home_override_set rather than the value: the override is a person's
	// directory NAME on a corporate share, which is their username as often as
	// not. Whether an admin pinned one is the governance fact; what they pinned
	// it to is in the row, readable by anyone who may read the row.
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"drive.grant.write", saved.ID.String(), "success", mustJSON(map[string]any{
			"subject_type":      saved.SubjectType,
			"subject":           saved.Subject,
			"drive_id":          saved.DriveID,
			"priority":          saved.Priority,
			"size_mib_override": saved.SizeMiBOverride,
			"writable_override": saved.WritableOverride,
			"home_override_set": saved.HomeOverride != "",
			"enabled":           saved.Enabled,
		})))
	writeJSON(w, status, saved)
}

// handleDeleteUserDriveGrant removes one allocation by id (204), 404 when
// unknown. This is the product-side half of OFFBOARDING — and the half that
// deletes no data, which is why the audit row carries the drive's declared
// `reclaim` intent: the log then says what the operator was told to do about
// the directory this row was the last pointer to. operatorOnly (routes.go).
func (s *Server) handleDeleteUserDriveGrant(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "user drive allocation")
	if !ok {
		return
	}
	// The DELETE hands back the row it removed, so the audit row is written from
	// the thing that was actually deleted rather than from a read taken beside
	// it. It used to be the latter — a ListUserDriveGrants scan for one id,
	// taken BEFORE the delete — and that shape had to load every allocation in
	// the deployment to describe one, and could describe a row a concurrent
	// write had since changed.
	deleted, err := s.cfg.Store.DeleteUserDriveGrant(r.Context(), id)
	if notFoundIf(w, err, "user drive allocation") {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete user drive grant: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"drive.grant.delete", id.String(), "success", s.userDriveGrantAuditData(r, deleted)))
	w.WriteHeader(http.StatusNoContent)
}

// userDriveGrantAuditData renders the deleted allocation's subject and its
// drive's reclaim intent for the audit row above.
//
// GetUserDrive is still read here, and stays: `reclaim` is the field that makes
// this row worth writing — it is what tells the offboarding runbook whether the
// directory this binding was the last pointer to should be kept or reclaimed —
// and it lives on the DRIVE, which the deleted grant only names by id. The read
// is best-effort: it happens after the delete succeeded, and a lookup failure
// must degrade the row's detail rather than turn a good delete into a 500.
func (s *Server) userDriveGrantAuditData(r *http.Request, g types.UserDriveGrant) []byte {
	out := map[string]any{
		"subject_type": g.SubjectType,
		"subject":      g.Subject,
		"drive_id":     g.DriveID,
	}
	if d, err := s.cfg.Store.GetUserDrive(r.Context(), g.DriveID); err == nil {
		out["drive"] = d.Name
		out["reclaim"] = d.Reclaim
	}
	return mustJSON(out)
}

// ─── /me ───────────────────────────────────────────────────────────────────

// meUserDrive is the /me.user_drive object: what the caller would mount if they
// asked for it on their next run, or nil when they would mount nothing.
//
// It is the MEMBER's view of the same resolution the run path takes, and it is
// keyed on the resolver rather than on role: a security admin's own drive
// resolves exactly like a member's, because a drive is per-principal and not
// per-tier. An operator with no per-human subjects (admin token, local mode)
// resolves to nil, which is the resolver's own step 2 and not a special case
// here.
//
// IT CARRIES NO DOOR FIELD, deliberately (owner ruling at the mock gate). This
// object means ONE thing — "what is allocated to you" — and the door is a
// property of the caller's PROFILE, not of the allocation. Folding them would
// make the two states that matter inexpressible: a member who is denied and has
// no allocation would be indistinguishable from a member who simply has none,
// which is exactly the pair the console has to tell apart. The door rides
// beside it as /me.user_drive_denied_by_profile.
type meUserDrive struct {
	Name        string                   `json:"name"`
	Backend     types.DriveBackend       `json:"backend"`
	SizeMiB     int                      `json:"size_mib,omitempty"`
	Writable    bool                     `json:"writable"`
	Enforcement types.StorageEnforcement `json:"enforcement"`
	HomeName    string                   `json:"home_name,omitempty"`
	// Paused: allocated, disabled by an admin — the chip says so and the New Run
	// card renders the paused line where the checkbox would be.
	Paused bool `json:"paused,omitempty"`
}

// resolveMeUserDrive answers the /me.user_drive field, or nil.
//
// EVERY FAILURE IS nil, and that is the one place in this feature where failing
// quiet is right: /me is a display read whose other fields the console needs to
// render the shell at all, so a store hiccup must degrade the drive chip rather
// than 500 the whole endpoint and log the human out of their own console. The
// ENFORCEMENT path (seedRequestDrive) makes the opposite choice for the same
// error, and must: mounting nothing where an admin allocated something is a
// data loss, while showing nothing for a moment is a refresh.
func (s *Server) resolveMeUserDrive(r *http.Request) *meUserDrive {
	resolved, err := s.resolveUserDrive(r.Context())
	if err != nil || resolved == nil {
		return nil
	}
	return &meUserDrive{
		Name:        resolved.Drive.Name,
		Backend:     resolved.Drive.Backend,
		SizeMiB:     resolved.SizeMiB,
		Writable:    resolved.Writable,
		Enforcement: resolved.Enforcement,
		HomeName:    resolved.HomeName,
		Paused:      resolved.Paused,
	}
}

// userDriveDeniedByProfile names the profile whose DenyUserDrive door is shut
// for this caller, or "" when the door is open — the /me sibling field
// user_drive_denied_by_profile.
//
// A STRING, not a bool, because the member-facing sentence quotes the profile
// by name ("your governance profile %q does not allow…") and a bool would make
// the console invent the rest of it or omit the one word that tells an admin
// which profile to look at.
//
// INDEPENDENT OF THE ALLOCATION, which is the point of the split: a member with
// no drive AND a shut door is a real, distinct state — asking their admin for
// an allocation would not help them, and a field folded into user_drive could
// not have said so.
//
// It reads driveDoorProfile — the SAME predicate denyMemberDrive enforces with,
// whose keying (operator exempt, unassigned member has no door) is documented
// there. A ceiling that cannot be resolved reports "" for resolveMeUserDrive's
// own reason — /me is a display read, and the ENFORCEMENT path answers the same
// failure with a refusal. The operator short-circuit stays HERE too, ahead of
// the resolve: a display read must not cost an operator a ceiling round-trip.
func (s *Server) userDriveDeniedByProfile(r *http.Request) string {
	if s.isOperator(r.Context()) {
		return ""
	}
	ceiling, err := s.effectiveCeiling(r.Context())
	if err != nil {
		return ""
	}
	name, _ := s.driveDoorProfile(r.Context(), ceiling)
	return name
}

// driveRefusal composes a 422 body in the frozen member voice: lowercase
// opening, prefixed `drive:`, and it names the remedy. One helper so the four
// refusal sites cannot drift into four different tones for one feature.
func driveRefusal(reason string) string {
	return "drive: " + strings.TrimSpace(reason)
}
