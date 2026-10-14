// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reclaimRunner is a substrate that CAN reclaim, and that records every call.
//
// The recording is the point on most of these cases: the assertion that
// matters for a destructive verb is not what the handler answered but whether
// it reached the storage at all, and only a runner that counts its calls can
// say "the refusal happened BEFORE the delete" rather than "the refusal
// happened".
type reclaimRunner struct {
	runner.Runner
	outcome runner.DriveReclaimOutcome
	err     error
	calls   []types.DriveMount
}

func (r *reclaimRunner) ReclaimDrive(_ context.Context, m types.DriveMount) (runner.DriveReclaimOutcome, error) {
	r.calls = append(r.calls, m)
	return r.outcome, r.err
}

// noReclaimRunner is a wired substrate WITHOUT the optional capability — a
// deployment on a runner that cannot destroy storage, which is the 501 arm.
type noReclaimRunner struct{ runner.Runner }

// reclaimServer is the super-admin server the destroy verb runs behind, over a
// deployment holding one drive and (optionally) one allocation.
func reclaimServer(t *testing.T, d types.UserDrive, g *types.UserDriveGrant, rn runner.Runner) (*Server, *driveCRUDStore, *recRecorder) {
	t.Helper()
	st := newDriveCRUDStore()
	st.drives[d.ID] = d
	if g != nil {
		st.grants[g.ID] = *g
	}
	audit := &recRecorder{}
	srv := New(Config{BaseCtx: testBaseCtx(t), Store: st, Audit: audit, Runner: rn, RunnerTarget: "docker", LocalMode: true})
	return srv, st, audit
}

// reclaimDrive is the managed docker_volume drive every case starts from: the
// backend this deployment's runner dispatches, so nothing is refused for a
// reason the case is not about.
func reclaimDrive(mut func(*types.UserDrive)) types.UserDrive {
	d := types.UserDrive{
		ID: uuid.New(), Name: "Corp NAS", Backend: types.DriveBackendDockerVolume,
		HomeTemplate: types.HomeTemplateHash, SizeMiB: 10240, Reclaim: types.DriveReclaimRetain,
	}
	if mut != nil {
		mut(&d)
	}
	return d
}

const reclaimSubject = "sub-drive-bob"

// reclaimCall posts the destroy verb for one subject against one drive id.
func reclaimCall(t *testing.T, srv *Server, id uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	return driveCall(t, srv.handleReclaimUserDrive, http.MethodPost,
		"/api/v1/drives/"+id.String()+"/reclaim", body, map[string]string{"id": id.String()})
}

// reclaimRow decodes the last drive.reclaim row's payload, and fails when there
// is none — "no row at all" is the failure this verb can least afford.
func reclaimRow(t *testing.T, rec *recRecorder) (types.AuditEvent, map[string]any) {
	t.Helper()
	ev := driveAuditEvent(t, rec, "drive.reclaim")
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("decode drive.reclaim payload: %v", err)
	}
	return ev, data
}

// TestReclaimUserDriveDestroysTheObjectAndNamesWhatWent is the happy path, and
// the assertion is as much about the RECORD as the deletion: the drive row and
// the allocation can both be gone by the time anyone reads the trail, so this
// row is the only durable statement of which bytes went.
func TestReclaimUserDriveDestroysTheObjectAndNamesWhatWent(t *testing.T) {
	d := reclaimDrive(nil)
	g := grantFixture(d.ID, func(g *types.UserDriveGrant) { g.Subject = reclaimSubject })
	rn := &reclaimRunner{outcome: runner.DriveReclaimDeleted}
	srv, _, audit := reclaimServer(t, d, g, rn)

	w := reclaimCall(t, srv, d.ID, `{"subject_type":"user","subject":"`+reclaimSubject+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("reclaim = %d, want 200: %s", w.Code, w.Body.String())
	}

	home, err := types.DriveHomeName(d, reclaimSubject, "")
	if err != nil {
		t.Fatalf("derive home: %v", err)
	}
	object := types.DriveObjectName(d, home)

	if len(rn.calls) != 1 {
		t.Fatalf("substrate calls = %d, want exactly 1", len(rn.calls))
	}
	if got := rn.calls[0].ObjectName; got != object {
		t.Errorf("the substrate was asked to destroy %q, want %q — naming the wrong object here deletes "+
			"somebody else's work", got, object)
	}
	if got := rn.calls[0].HomeName; got != home {
		t.Errorf("mount home = %q, want %q", got, home)
	}
	if got := rn.calls[0].Backend; got != d.Backend {
		t.Errorf("mount backend = %q, want %q", got, d.Backend)
	}

	var res userDriveReclaimResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if res.Outcome != string(runner.DriveReclaimDeleted) || res.Object != object || res.DriveID != d.ID {
		t.Errorf("response = %+v, want outcome=deleted object=%q drive_id=%s", res, object, d.ID)
	}

	ev, data := reclaimRow(t, audit)
	if ev.Outcome != "success" {
		t.Errorf("event outcome = %q, want success", ev.Outcome)
	}
	if ev.Target != d.ID.String() {
		t.Errorf("event target = %q, want the drive id %s", ev.Target, d.ID)
	}
	// The payload keys are a CONTRACT with docs/AUDIT-ACTIONS.md, which names
	// exactly these seven. A row that quietly dropped `object` would still read
	// as an audited destruction while naming nothing that was destroyed.
	want := map[string]any{
		"backend": string(d.Backend), "drive": d.Name, "drive_id": d.ID.String(),
		"object": object, "outcome": "deleted", "subject": reclaimSubject, "subject_type": "user",
	}
	for k, v := range want {
		if data[k] != v {
			t.Errorf("drive.reclaim data[%q] = %v, want %v", k, data[k], v)
		}
	}
	if len(data) != len(want) {
		t.Errorf("drive.reclaim payload has %d keys (%v), want exactly the %d documented in "+
			"docs/AUDIT-ACTIONS.md", len(data), data, len(want))
	}
}

// TestReclaimUserDriveTellsAlreadyAbsentFromDeleted pins the distinction the
// outcome field exists for. Both are 200s and both are `success` events, so
// only this field separates "this call destroyed a person's storage" from "it
// was already gone when we looked" — and an offboarding review needs the
// second question answered, not the first.
func TestReclaimUserDriveTellsAlreadyAbsentFromDeleted(t *testing.T) {
	d := reclaimDrive(nil)
	rn := &reclaimRunner{outcome: runner.DriveReclaimAlreadyAbsent}
	srv, _, audit := reclaimServer(t, d, nil, rn)

	w := reclaimCall(t, srv, d.ID, `{"subject_type":"user","subject":"`+reclaimSubject+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("reclaim = %d, want 200: %s", w.Code, w.Body.String())
	}
	ev, data := reclaimRow(t, audit)
	if ev.Outcome != "success" {
		t.Errorf("event outcome = %q, want success — nothing failed", ev.Outcome)
	}
	if data["outcome"] != string(runner.DriveReclaimAlreadyAbsent) {
		t.Errorf("data outcome = %v, want already_absent — reporting this as `deleted` would put a "+
			"destruction in the trail that never happened", data["outcome"])
	}
}

// TestReclaimUserDriveRefusesWhileARunHoldsIt is the 409 the issue asks for by
// name, and the audit half is the reason it is a separate case: a refusal is a
// super-admin ASKING for a named person's storage to be destroyed, and a
// success-only trail hides exactly the pattern an incident review looks for.
func TestReclaimUserDriveRefusesWhileARunHoldsIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"a sandbox still holds the object", runner.ErrDriveInUse},
		{"the object under this name is not this drive's", runner.ErrDriveNotReclaimable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := reclaimDrive(nil)
			rn := &reclaimRunner{err: tc.err}
			srv, _, audit := reclaimServer(t, d, nil, rn)

			w := reclaimCall(t, srv, d.ID, `{"subject_type":"user","subject":"`+reclaimSubject+`"}`)
			if w.Code != http.StatusConflict {
				t.Fatalf("reclaim = %d, want 409: %s", w.Code, w.Body.String())
			}
			ev, data := reclaimRow(t, audit)
			if ev.Outcome != "failure" {
				t.Errorf("event outcome = %q, want failure", ev.Outcome)
			}
			if data["outcome"] != driveReclaimOutcomeRefused {
				t.Errorf("data outcome = %v, want %q", data["outcome"], driveReclaimOutcomeRefused)
			}
			if data["subject"] != reclaimSubject {
				t.Errorf("the refusal row does not name WHOSE storage was asked for: %v", data)
			}
		})
	}
}

// TestReclaimUserDriveAuditsASubstrateFailure: a 500 is audited too. The
// operator has to be able to tell "the substrate said no" from "the substrate
// broke", and on Kubernetes the second is what a stock install answers — the
// apiserver's 403 for a `delete` verb the chart does not grant.
func TestReclaimUserDriveAuditsASubstrateFailure(t *testing.T) {
	d := reclaimDrive(nil)
	rn := &reclaimRunner{err: errors.New("persistentvolumeclaims is forbidden")}
	srv, _, audit := reclaimServer(t, d, nil, rn)

	w := reclaimCall(t, srv, d.ID, `{"subject_type":"user","subject":"`+reclaimSubject+`"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("reclaim = %d, want 500: %s", w.Code, w.Body.String())
	}
	ev, data := reclaimRow(t, audit)
	if ev.Outcome != "failure" || data["outcome"] != driveReclaimOutcomeFailed {
		t.Errorf("event=%q data outcome=%v, want failure/%q", ev.Outcome, data["outcome"], driveReclaimOutcomeFailed)
	}
	// SF-13: the substrate's own error text is audited/logged, never handed to
	// the member in the 500 body.
	if strings.Contains(w.Body.String(), "persistentvolumeclaims is forbidden") {
		t.Errorf("500 body must not carry the substrate's error text, got %s", w.Body.String())
	}
}

// TestReclaimUserDriveRefusesATierThatNamesNoSingleObject: a group or `all`
// allocation gives every person it matches their OWN object, so it names no
// one thing to destroy. The two ways a machine could answer this are "guess
// one" and "delete them all"; neither is a decision code may make.
func TestReclaimUserDriveRefusesATierThatNamesNoSingleObject(t *testing.T) {
	for _, body := range []string{
		`{"subject_type":"group","subject":"engineering"}`,
		`{"subject_type":"all","subject":""}`,
	} {
		d := reclaimDrive(nil)
		rn := &reclaimRunner{outcome: runner.DriveReclaimDeleted}
		srv, _, audit := reclaimServer(t, d, nil, rn)

		w := reclaimCall(t, srv, d.ID, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("reclaim %s = %d, want 400: %s", body, w.Code, w.Body.String())
		}
		if len(rn.calls) != 0 {
			t.Errorf("reclaim %s reached the substrate %d times — a tier-wide delete is the failure this "+
				"refusal exists to make impossible", body, len(rn.calls))
		}
		if len(audit.events) != 0 {
			t.Errorf("reclaim %s wrote %v — nothing was attempted against any storage", body, driveAuditActions(audit))
		}
	}
}

// TestReclaimUserDriveRefusesStorageItDidNotAllocate is the no-rm-rf rail. A
// host_path home is a directory inside a tree the OPERATOR mounted and named;
// Wardyn created it and must delete it neither by API nor by privilege.
func TestReclaimUserDriveRefusesStorageItDidNotAllocate(t *testing.T) {
	d := reclaimDrive(func(d *types.UserDrive) {
		d.Backend, d.HomeTemplate, d.HostRoot = types.DriveBackendHostPath, types.HomeTemplateSub, "/srv/shares"
	})
	rn := &reclaimRunner{outcome: runner.DriveReclaimDeleted}
	srv, _, audit := reclaimServer(t, d, nil, rn)

	w := reclaimCall(t, srv, d.ID, `{"subject_type":"user","subject":"`+reclaimSubject+`"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("reclaim of a share = %d, want 422: %s", w.Code, w.Body.String())
	}
	if len(rn.calls) != 0 {
		t.Errorf("a share reached the substrate: %+v — there is no recursive delete in this product, at any "+
			"privilege, for any backend", rn.calls)
	}
	if len(audit.events) != 0 {
		t.Errorf("a refusal that never addressed an object wrote %v", driveAuditActions(audit))
	}
}

// TestReclaimUserDriveRefusesABackendThisDeploymentDoesNotRun: a k8s drive on a
// Docker install names an object no local daemon has ever heard of. The honest
// answer is "not here" — never a delete aimed at a name that might match
// something else on the substrate that IS wired.
func TestReclaimUserDriveRefusesABackendThisDeploymentDoesNotRun(t *testing.T) {
	d := reclaimDrive(func(d *types.UserDrive) { d.Backend = types.DriveBackendK8sPVC })
	rn := &reclaimRunner{outcome: runner.DriveReclaimDeleted}
	srv, _, _ := reclaimServer(t, d, nil, rn) // RunnerTarget is "docker"

	w := reclaimCall(t, srv, d.ID, `{"subject_type":"user","subject":"`+reclaimSubject+`"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("reclaim = %d, want 422: %s", w.Code, w.Body.String())
	}
	if len(rn.calls) != 0 {
		t.Errorf("a k8s drive was reclaimed through the docker substrate: %+v", rn.calls)
	}
}

// TestReclaimUserDriveIsUnimplementedWithoutACapableSubstrate: 501, and NOTHING
// audited — no attempt reached any storage, so a row would describe an act that
// did not happen.
func TestReclaimUserDriveIsUnimplementedWithoutACapableSubstrate(t *testing.T) {
	d := reclaimDrive(nil)
	srv, _, audit := reclaimServer(t, d, nil, noReclaimRunner{})

	w := reclaimCall(t, srv, d.ID, `{"subject_type":"user","subject":"`+reclaimSubject+`"}`)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("reclaim = %d, want 501: %s", w.Code, w.Body.String())
	}
	if len(audit.events) != 0 {
		t.Errorf("a call that never reached a substrate wrote %v", driveAuditActions(audit))
	}
}

// TestReclaimUserDriveIsNotFoundForAnUnknownDrive.
func TestReclaimUserDriveIsNotFoundForAnUnknownDrive(t *testing.T) {
	d := reclaimDrive(nil)
	rn := &reclaimRunner{outcome: runner.DriveReclaimDeleted}
	srv, _, _ := reclaimServer(t, d, nil, rn)

	w := reclaimCall(t, srv, uuid.New(), `{"subject_type":"user","subject":"`+reclaimSubject+`"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("reclaim of an unknown drive = %d, want 404: %s", w.Code, w.Body.String())
	}
	if len(rn.calls) != 0 {
		t.Errorf("an unknown drive still addressed an object: %+v", rn.calls)
	}
}

// TestReclaimUserDriveHonoursAPinnedHome is the case that decides whether this
// verb destroys the right bytes. An object name folds the drive slug with the
// HOME, and the home is either derived from the person or PINNED on their
// allocation — so deriving one without reading the other names a different
// object, which here means deleting somebody else's directory.
func TestReclaimUserDriveHonoursAPinnedHome(t *testing.T) {
	d := reclaimDrive(nil)
	g := grantFixture(d.ID, func(g *types.UserDriveGrant) {
		g.Subject, g.HomeOverride = reclaimSubject, "bobs-pinned-home"
	})
	rn := &reclaimRunner{outcome: runner.DriveReclaimDeleted}
	srv, _, _ := reclaimServer(t, d, g, rn)

	if w := reclaimCall(t, srv, d.ID, `{"subject_type":"user","subject":"`+reclaimSubject+`"}`); w.Code != http.StatusOK {
		t.Fatalf("reclaim = %d, want 200: %s", w.Code, w.Body.String())
	}
	want := types.DriveObjectName(d, "bobs-pinned-home")
	if len(rn.calls) != 1 || rn.calls[0].ObjectName != want {
		t.Fatalf("the substrate was asked for %+v, want the PINNED object %q — the derived name is a "+
			"different person's directory", rn.calls, want)
	}
	// The control: the same drive with no override derives the template's name
	// instead, so the assertion above is about the override and not about the
	// fixture happening to match.
	derived, err := types.DriveHomeName(d, reclaimSubject, "")
	if err != nil {
		t.Fatalf("derive home: %v", err)
	}
	if types.DriveObjectName(d, derived) == want {
		t.Fatal("the pinned and derived object names are identical, so this case proves nothing")
	}
}

// TestReclaimUserDriveFailsClosedWhenTheAllocationCannotBeRead: "I could not
// find out whether a directory name was pinned" must never become "there
// wasn't one" on the one path that then destroys the object.
func TestReclaimUserDriveFailsClosedWhenTheAllocationCannotBeRead(t *testing.T) {
	d := reclaimDrive(nil)
	rn := &reclaimRunner{outcome: runner.DriveReclaimDeleted}
	srv, st, audit := reclaimServer(t, d, nil, rn)
	st.listErr = errors.New("connection reset")

	w := reclaimCall(t, srv, d.ID, `{"subject_type":"user","subject":"`+reclaimSubject+`"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("reclaim = %d, want 500: %s", w.Code, w.Body.String())
	}
	if len(rn.calls) != 0 {
		t.Errorf("an object was destroyed although the pinned-home read failed: %+v", rn.calls)
	}
	if len(audit.events) != 0 {
		t.Errorf("a call that addressed no object wrote %v", driveAuditActions(audit))
	}
	// SF-13: the store's own error text (here, a plain driver message, but a
	// real one is pgx/driver text) is logged, never handed to the member.
	if strings.Contains(w.Body.String(), "connection reset") {
		t.Errorf("500 body must not carry the store's error text, got %s", w.Body.String())
	}
}

// TestReclaimUserDriveHasNoConsoleButton is the fourth rail as a FORWARD GUARD
// rather than an intention. A destructive confirmation is a screen, this one
// has no approved mock, so the surface is the API and the CLI — and the way
// that decision survives the next UI change is a test that fails when a caller
// appears in the console source.
func TestReclaimUserDriveHasNoConsoleButton(t *testing.T) {
	root := ".."
	for depth := 0; depth < 3; depth++ {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		root = filepath.Join(root, "..")
	}
	src := filepath.Join(root, "ui", "src")
	if _, err := os.Stat(src); err != nil {
		t.Skipf("no console source tree at %s", src)
	}
	var offenders []string
	err := filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		switch filepath.Ext(path) {
		case ".ts", ".tsx":
		default:
			return nil
		}
		b, readErr := os.ReadFile(path) //nolint:gosec // a walked path under ui/src
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(b), "/reclaim") {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", src, err)
	}
	if len(offenders) != 0 {
		t.Errorf("the console calls the drive destroy verb in %v — POST /drives/{id}/reclaim is API and CLI "+
			"only in 0.8: a destructive confirmation is a screen, and this one has no approved mock "+
			"(docs/design/CONSOLE-RULES.md's mock-first rule). Get the mock approved first", offenders)
	}
}

// TestReclaimUserDriveKeysOnTheSubjectItIsGiven pins today's derivation for an
// allocation keyed by the sign-in subject: with no home_subject, the hash home
// and the object's subject label both come from `subject`.
func TestReclaimUserDriveKeysOnTheSubjectItIsGiven(t *testing.T) {
	for _, withGrant := range []bool{true, false} {
		d := reclaimDrive(nil)
		var g *types.UserDriveGrant
		if withGrant {
			g = grantFixture(d.ID, func(g *types.UserDriveGrant) { g.Subject = reclaimSubject })
		}
		rn := &reclaimRunner{outcome: runner.DriveReclaimDeleted}
		srv, _, _ := reclaimServer(t, d, g, rn)
		if w := reclaimCall(t, srv, d.ID, `{"subject_type":"user","subject":"`+reclaimSubject+`"}`); w.Code != http.StatusOK {
			t.Fatalf("allocation present=%v: reclaim = %d, want 200: %s", withGrant, w.Code, w.Body.String())
		}
		home, err := types.DriveHomeName(d, reclaimSubject, "")
		if err != nil {
			t.Fatalf("derive home: %v", err)
		}
		if len(rn.calls) != 1 || rn.calls[0].ObjectName != types.DriveObjectName(d, home) ||
			rn.calls[0].SubjectHash != types.DriveSubjectHash(reclaimSubject) {
			t.Errorf("allocation present=%v: substrate asked for %+v, want the object and label of %q", withGrant, rn.calls, reclaimSubject)
		}
	}
}

// TestReclaimUserDriveDerivesFromTheHomeSubject: an allocation keyed by an
// email resolves at run time to the hash of the person's SIGN-IN subject, with
// that subject's label. home_subject carries it, so the reclaim names the same
// object the run mounted; `subject` still finds the allocation and any pinned
// directory name on it.
func TestReclaimUserDriveDerivesFromTheHomeSubject(t *testing.T) {
	const email, sub = "alice@corp.example", "sub-alice"
	for _, pinned := range []string{"", "alice-pinned"} {
		d := reclaimDrive(nil)
		g := grantFixture(d.ID, func(g *types.UserDriveGrant) { g.Subject, g.HomeOverride = email, pinned })
		rn := &reclaimRunner{outcome: runner.DriveReclaimDeleted}
		srv, _, _ := reclaimServer(t, d, g, rn)
		w := reclaimCall(t, srv, d.ID, `{"subject_type":"user","subject":"Alice@Corp.Example","home_subject":"Sub-Alice"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("pinned=%q: reclaim = %d, want 200: %s", pinned, w.Code, w.Body.String())
		}
		home, err := types.DriveHomeName(d, sub, pinned)
		if err != nil {
			t.Fatalf("derive home: %v", err)
		}
		if emailHome, _ := types.DriveHomeName(d, email, pinned); pinned == "" && emailHome == home {
			t.Fatal("the email and sign-in homes are identical, so this case proves nothing")
		}
		if len(rn.calls) != 1 || rn.calls[0].ObjectName != types.DriveObjectName(d, home) ||
			rn.calls[0].SubjectHash != types.DriveSubjectHash(sub) {
			t.Errorf("pinned=%q: substrate asked for %+v, want object %q labelled for the sign-in subject",
				pinned, rn.calls, types.DriveObjectName(d, home))
		}
	}
	// A home_subject the allocation key rules refuse is a 400, before any storage call.
	d := reclaimDrive(nil)
	rn := &reclaimRunner{outcome: runner.DriveReclaimDeleted}
	srv, _, _ := reclaimServer(t, d, nil, rn)
	if w := reclaimCall(t, srv, d.ID, `{"subject_type":"user","subject":"`+email+`","home_subject":"bad\u0007sub"}`); w.Code != http.StatusBadRequest || len(rn.calls) != 0 {
		t.Errorf("unusable home_subject = %d with %d storage calls, want 400 and none: %s", w.Code, len(rn.calls), w.Body.String())
	}
}

// TestReclaimUserDriveChecksAFoundKeyAgainstTheDirectory: with no home_subject,
// an allocation found under key A derives the home from A only when the principal
// directory names exactly one principal for A and that principal folds to A.
// Otherwise the operator must pass home_subject. A key nobody is recorded under,
// or no allocation at all, keeps the derivation from the key, and an unreadable or
// absent directory refuses.
func TestReclaimUserDriveChecksAFoundKeyAgainstTheDirectory(t *testing.T) {
	for _, tc := range []struct {
		name       string
		key        string // the allocation's stored subject; "" means no allocation
		subject    string // the request's subject
		principals []string
		lookupErr  error
		want       int
		wantLookup bool
	}{
		{"a sign-in subject with an @ that is its own principal", "alice@idp.example", "alice@idp.example", []string{"alice@idp.example"}, nil, http.StatusOK, true},
		{"a principal that differs only in ASCII case", "sub-alice", "sub-alice", []string{"Sub-Alice"}, nil, http.StatusOK, true},
		{"an alias of another principal", "alice@corp.example", "alice@corp.example", []string{"sub-alice"}, nil, http.StatusBadRequest, true},
		{"a key that names several principals", "alice@corp.example", "alice@corp.example", []string{"sub-a", "sub-b"}, nil, http.StatusBadRequest, true},
		{"a key nobody is recorded under", "sub-ghost", "sub-ghost", nil, nil, http.StatusOK, true},
		{"no allocation: the key is not looked up", "", "alice@corp.example", []string{"sub-alice"}, nil, http.StatusOK, false},
		{"an unreadable directory", "sub-alice", "sub-alice", nil, errors.New("directory down"), http.StatusInternalServerError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := reclaimDrive(nil)
			var g *types.UserDriveGrant
			if tc.key != "" {
				g = grantFixture(d.ID, func(g *types.UserDriveGrant) { g.Subject = tc.key })
			}
			rn := &reclaimRunner{outcome: runner.DriveReclaimDeleted}
			srv, st, _ := reclaimServer(t, d, g, rn)
			st.principals = map[string][]string{tc.subject: tc.principals}
			st.principalsErr = tc.lookupErr
			w := reclaimCall(t, srv, d.ID, `{"subject_type":"user","subject":"`+tc.subject+`"}`)
			if w.Code != tc.want {
				t.Fatalf("reclaim = %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if looked := len(st.lookups) > 0; looked != tc.wantLookup {
				t.Errorf("directory consulted = %v, want %v", looked, tc.wantLookup)
			}
			if tc.want != http.StatusOK {
				if len(rn.calls) != 0 {
					t.Errorf("storage was called on a refusal: %+v", rn.calls)
				}
				if tc.want == http.StatusBadRequest && !strings.Contains(w.Body.String(), "home_subject") {
					t.Errorf("400 body = %s, want it to name home_subject", w.Body.String())
				}
				return
			}
			home, err := types.DriveHomeName(d, tc.subject, "")
			if err != nil {
				t.Fatalf("derive home: %v", err)
			}
			if len(rn.calls) != 1 || rn.calls[0].ObjectName != types.DriveObjectName(d, home) ||
				rn.calls[0].SubjectHash != types.DriveSubjectHash(tc.subject) {
				t.Errorf("substrate asked for %+v, want the object and label of %q", rn.calls, tc.subject)
			}
		})
	}

	// An explicit home_subject is the operator's assertion: no lookup.
	d := reclaimDrive(nil)
	g := grantFixture(d.ID, func(g *types.UserDriveGrant) { g.Subject = "alice@corp.example" })
	rn := &reclaimRunner{outcome: runner.DriveReclaimDeleted}
	srv, st, _ := reclaimServer(t, d, g, rn)
	st.principals = map[string][]string{"alice@corp.example": {"sub-a", "sub-b"}}
	if w := reclaimCall(t, srv, d.ID, `{"subject_type":"user","subject":"alice@corp.example","home_subject":"sub-alice"}`); w.Code != http.StatusOK || len(st.lookups) != 0 {
		t.Errorf("explicit home_subject = %d with lookups %v, want 200 and none: %s", w.Code, st.lookups, w.Body.String())
	}

	// A store with no principal directory refuses a found key rather than guess.
	srv.cfg.Store = struct{ store.Store }{st}
	rn.calls = nil
	if w := reclaimCall(t, srv, d.ID, `{"subject_type":"user","subject":"alice@corp.example"}`); w.Code != http.StatusInternalServerError || len(rn.calls) != 0 {
		t.Errorf("no directory = %d with %d storage calls, want 500 and none: %s", w.Code, len(rn.calls), w.Body.String())
	}
}
