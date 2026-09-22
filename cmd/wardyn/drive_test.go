// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// fakeDriveServer models just enough of the real /drives family
// (internal/api/user_drives.go) to make a round trip through it MEAN
// something: POST /drives refuses a name another drive already holds (409,
// the real UNIQUE(name) behavior) rather than silently accepting a second
// row, and POST /drives/grants upserts by the natural key (subject_type,
// subject) rather than accumulating a row per call. A test that only echoed
// back whatever it received would pass even if ApplyDrives sent POST twice
// for an already-created drive; this one would 409.
type fakeDriveServer struct {
	drives map[uuid.UUID]sdk.UserDrive
	grants map[string]sdk.UserDriveGrant // key: subject_type+"\x00"+subject
}

func newFakeDriveServer() *fakeDriveServer {
	return &fakeDriveServer{
		drives: map[uuid.UUID]sdk.UserDrive{},
		grants: map[string]sdk.UserDriveGrant{},
	}
}

func (f *fakeDriveServer) grantKey(subjectType sdk.CapabilitySubjectType, subject string) string {
	return string(subjectType) + "\x00" + subject
}

func (f *fakeDriveServer) document() sdk.DrivesDocument {
	doc := sdk.DrivesDocument{HostRootsConfigured: true, RunnerTarget: "docker"}
	for _, d := range f.drives {
		count := 0
		for _, g := range f.grants {
			if g.DriveID == d.ID {
				count++
			}
		}
		doc.Drives = append(doc.Drives, sdk.UserDriveListItem{UserDrive: d, GrantCount: count})
	}
	for _, g := range f.grants {
		doc.Grants = append(doc.Grants, g)
	}
	doc.GrantTotal = len(doc.Grants)
	// Stable order so two documents built from the same map state compare
	// equal regardless of Go's randomized map iteration.
	sortDrives(doc.Drives)
	sortGrants(doc.Grants)
	return doc
}

func sortDrives(d []sdk.UserDriveListItem) {
	for i := 1; i < len(d); i++ {
		for j := i; j > 0 && d[j].Name < d[j-1].Name; j-- {
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
}

func sortGrants(g []sdk.UserDriveGrant) {
	for i := 1; i < len(g); i++ {
		for j := i; j > 0 && g[j].Subject < g[j-1].Subject; j-- {
			g[j], g[j-1] = g[j-1], g[j]
		}
	}
}

func (f *fakeDriveServer) nameConflict(id uuid.UUID, name string) bool {
	for existingID, d := range f.drives {
		if existingID != id && d.Name == name {
			return true
		}
	}
	return false
}

func (f *fakeDriveServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/drives":
			_ = json.NewEncoder(w).Encode(f.document())

		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/drives":
			var req sdk.DriveRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if f.nameConflict(uuid.Nil, req.Name) {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "a user drive named " + req.Name + " already exists"})
				return
			}
			d := sdk.UserDrive{ID: uuid.New(), Name: req.Name, Backend: req.Backend,
				HostRoot: req.HostRoot, StorageClass: req.StorageClass, HomeTemplate: req.HomeTemplate,
				SizeMiB: req.SizeMiB, Writable: req.Writable, Reclaim: req.Reclaim}
			f.drives[d.ID] = d
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(d)

		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/v1/drives/"):
			id, err := uuid.Parse(strings.TrimPrefix(r.URL.Path, "/api/v1/drives/"))
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var req sdk.DriveRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if f.nameConflict(id, req.Name) {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "a user drive named " + req.Name + " already exists"})
				return
			}
			d := sdk.UserDrive{ID: id, Name: req.Name, Backend: req.Backend,
				HostRoot: req.HostRoot, StorageClass: req.StorageClass, HomeTemplate: req.HomeTemplate,
				SizeMiB: req.SizeMiB, Writable: req.Writable, Reclaim: req.Reclaim}
			f.drives[id] = d
			_ = json.NewEncoder(w).Encode(d)

		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/drives/grants":
			var req sdk.DriveGrantRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			key := f.grantKey(req.SubjectType, req.Subject)
			existing, had := f.grants[key]
			g := sdk.UserDriveGrant{
				SubjectType: req.SubjectType, Subject: req.Subject, DriveID: req.DriveID,
				Priority: req.Priority, SizeMiBOverride: req.SizeMiBOverride,
				WritableOverride: req.WritableOverride, Enabled: req.Enabled == nil || *req.Enabled,
			}
			if req.HomeOverride != nil {
				g.HomeOverride = *req.HomeOverride
			}
			if had {
				g.ID = existing.ID
				w.WriteHeader(http.StatusOK)
			} else {
				g.ID = uuid.New()
				w.WriteHeader(http.StatusCreated)
			}
			f.grants[key] = g
			_ = json.NewEncoder(w).Encode(g)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// runDriveGet runs `drive get` against url and returns what it printed and
// any error. `--json` output goes through emitJSON, which targets os.Stdout
// directly rather than cobra's OutOrStdout sink (run_wait_ready_test.go,
// sshkey_test.go note the same thing) — captureStdout (commands_test.go) is
// this tree's existing fix for that.
func runDriveGet(t *testing.T, url string) (stdout string, err error) {
	t.Helper()
	root := rootCmd()
	root.SetArgs([]string{"drive", "get", "--url", url, "--token", "tok"})
	root.SetErr(&strings.Builder{})
	stdout = captureStdout(t, func() { err = root.Execute() })
	return stdout, err
}

// runDriveApply marshals doc to a temp file, runs `drive apply` on it, and
// returns what it printed and any error (see runDriveGet on why stdout needs
// captureStdout).
func runDriveApply(t *testing.T, url string, doc sdk.DrivesDocument) (stdout string, err error) {
	t.Helper()
	b, merr := json.Marshal(doc)
	if merr != nil {
		t.Fatalf("marshal doc: %v", merr)
	}
	path := filepath.Join(t.TempDir(), "drives.json")
	if werr := os.WriteFile(path, b, 0o600); werr != nil {
		t.Fatalf("write doc: %v", werr)
	}
	root := rootCmd()
	root.SetArgs([]string{"drive", "apply", path, "--url", url, "--token", "tok"})
	root.SetErr(&strings.Builder{})
	stdout = captureStdout(t, func() { err = root.Execute() })
	return stdout, err
}

// seedDrives exercises every types.DriveBackend, not just docker_volume, the
// easy one.
func seedDrives() []sdk.UserDriveListItem {
	drives := []sdk.UserDrive{
		{Name: "corp-nas", Backend: sdk.DriveBackendHostPath, HostRoot: "/srv/homes",
			HomeTemplate: sdk.HomeTemplateSub, SizeMiB: 10240, Writable: true, Reclaim: sdk.DriveReclaimRetain},
		{Name: "scratch-vol", Backend: sdk.DriveBackendDockerVolume,
			HomeTemplate: sdk.HomeTemplateHash, SizeMiB: 2048, Writable: true, Reclaim: sdk.DriveReclaimDelete},
		{Name: "k8s-managed", Backend: sdk.DriveBackendK8sPVC, StorageClass: "fast-ssd",
			HomeTemplate: sdk.HomeTemplateHash, SizeMiB: 5120, Writable: false, Reclaim: sdk.DriveReclaimDelete},
		{Name: "k8s-static-share", Backend: sdk.DriveBackendK8sPVCStatic,
			HomeTemplate: sdk.HomeTemplateEmailLocal, SizeMiB: 1024, Writable: true, Reclaim: sdk.DriveReclaimRetain},
	}
	out := make([]sdk.UserDriveListItem, len(drives))
	for i, d := range drives {
		out[i] = sdk.UserDriveListItem{UserDrive: d}
	}
	return out
}

// seedGrants exercises every types.CapabilitySubjectType plus the tri-state
// override fields (writable_override, home_override, a disabled grant) that
// only a hand-built document can carry on a fresh allocation.
func seedGrants(driveByName map[string]uuid.UUID) []sdk.UserDriveGrant {
	falseVal := false
	return []sdk.UserDriveGrant{
		{SubjectType: sdk.CapabilitySubjectUser, Subject: "alice", DriveID: driveByName["corp-nas"],
			WritableOverride: &falseVal, HomeOverride: "alice-home", Enabled: true},
		{SubjectType: sdk.CapabilitySubjectGroup, Subject: "eng", DriveID: driveByName["k8s-managed"],
			Priority: 5, SizeMiBOverride: 4096, Enabled: true},
		{SubjectType: sdk.CapabilitySubjectAll, Subject: "", DriveID: driveByName["scratch-vol"],
			Enabled: false},
	}
}

// TestDriveApply_GetApplyRoundTripIsANoOp is the issue's stated acceptance:
// `wardyn drive get > f && wardyn drive apply f` changes nothing. It proves
// this over a document that exercises every backend (docker_volume,
// host_path, k8s_pvc, k8s_pvc_static) and every subject tier (user, group,
// all), against a fake server that enforces the real conflict rules — so a
// bug that made ApplyDrives re-POST an already-created drive (rather than PUT
// it by the id `get` returned) would 409 here, not pass silently.
func TestDriveApply_GetApplyRoundTripIsANoOp(t *testing.T) {
	fake := newFakeDriveServer()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	// Bootstrap the drives (every id zero, so this CREATES all four), then
	// read back the real ids the server minted — the same order a human
	// authoring a brand-new file would have to follow, since a grant names
	// its drive by id and none exists before the drive does.
	if _, err := runDriveApply(t, srv.URL, sdk.DrivesDocument{Drives: seedDrives()}); err != nil {
		t.Fatalf("bootstrap drives: %v", err)
	}
	if len(fake.drives) != 4 {
		t.Fatalf("bootstrap created %d drives, want 4", len(fake.drives))
	}
	byName := map[string]uuid.UUID{}
	for id, d := range fake.drives {
		byName[d.Name] = id
	}

	// Bootstrap the grants against those real ids.
	if _, err := runDriveApply(t, srv.URL, sdk.DrivesDocument{Grants: seedGrants(byName)}); err != nil {
		t.Fatalf("bootstrap grants: %v", err)
	}
	if len(fake.grants) != 3 {
		t.Fatalf("bootstrap created %d grants, want 3", len(fake.grants))
	}

	// get: snapshot the now-real state (real ids, every field the server
	// stored).
	getOut, err := runDriveGet(t, srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var before sdk.DrivesDocument
	if err := json.Unmarshal([]byte(getOut), &before); err != nil {
		t.Fatalf("unmarshal get output: %v", err)
	}
	if len(before.Drives) != 4 || len(before.Grants) != 3 {
		t.Fatalf("get returned %d drives / %d grants, want 4 / 3", len(before.Drives), len(before.Grants))
	}
	for _, d := range before.Drives {
		if d.ID == uuid.Nil {
			t.Fatalf("drive %q came back with a zero id — apply cannot round-trip it", d.Name)
		}
	}

	// apply(get()): re-applying the file `get` just produced must be a no-op
	// — same drive ids (PUT, not a second POST that would 409 on the name),
	// same grant ids (upserted by natural key), same field values throughout.
	if _, err := runDriveApply(t, srv.URL, before); err != nil {
		t.Fatalf("round-trip apply: %v", err)
	}
	if len(fake.drives) != 4 {
		t.Fatalf("round-trip apply left %d drives, want 4 (a duplicate would mean apply POSTed instead of PUT)", len(fake.drives))
	}
	if len(fake.grants) != 3 {
		t.Fatalf("round-trip apply left %d grants, want 3", len(fake.grants))
	}

	afterOut, err := runDriveGet(t, srv.URL)
	if err != nil {
		t.Fatalf("post-round-trip get: %v", err)
	}
	var after sdk.DrivesDocument
	if err := json.Unmarshal([]byte(afterOut), &after); err != nil {
		t.Fatalf("unmarshal post-round-trip get output: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("get before apply(get()) != get after — the round trip changed something.\nbefore: %+v\nafter:  %+v", before, after)
	}
}

// TestDriveApply_RejectsUnknownField pins the same strict-decode contract
// site-config's apply has: `apply` upserts exactly what the file states, so a
// typo'd key must be a parse error, not a silently dropped field.
func TestDriveApply_RejectsUnknownField(t *testing.T) {
	fake := newFakeDriveServer()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	path := filepath.Join(t.TempDir(), "drives.json")
	if err := os.WriteFile(path, []byte(`{"drivs":[]}`), 0o600); err != nil {
		t.Fatalf("write doc: %v", err)
	}
	root := rootCmd()
	root.SetArgs([]string{"drive", "apply", path, "--url", srv.URL, "--token", "tok"})
	root.SetOut(&strings.Builder{})
	root.SetErr(&strings.Builder{})
	err := root.Execute()
	if err == nil {
		t.Fatal("apply with an unknown field succeeded, want a decode error")
	}
	if !strings.Contains(err.Error(), "drivs") {
		t.Errorf("error %q does not name the unknown field", err)
	}
}
