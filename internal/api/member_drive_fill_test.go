// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fillHolderRunner is a substrate that answers the drive holder read: holder, or err.
type fillHolderRunner struct {
	memberDriveRunner
	holder string
	err    error
	asked  []types.DriveMount
}

func (r *fillHolderRunner) DriveHolder(_ context.Context, m types.DriveMount) (string, error) {
	r.asked = append(r.asked, m)
	return r.holder, r.err
}

const fillWorkspace = "6f1c0a52-6c6a-4b0e-9d52-0a5d9c1d7e11"

// fillDrive creates Alice's drive "Notes" and returns its id.
func fillDrive(t *testing.T, srv *Server) string {
	t.Helper()
	w := createMemberDrive(srv, personCtx(aliceSub), "Notes")
	var d struct {
		ID string `json:"id"`
	}
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &d) != nil || d.ID == "" {
		t.Fatalf("create the drive: %d %s", w.Code, w.Body.String())
	}
	return d.ID
}

func fillCall(srv *Server, ctx context.Context, id, body string) *httptest.ResponseRecorder {
	return memberCall(ctx, srv.handleFillMyDrive, http.MethodPost, "/api/v1/me/drives/"+id+"/seed", body, map[string]string{"id": id})
}

func fillBody(repo, into string) string {
	b, _ := json.Marshal(map[string]string{"workspace_id": fillWorkspace, "repo": repo, "into": into})
	return string(b)
}

// seedRows is the drive.seed rows recorded so far, each with its data decoded.
func seedRows(t *testing.T, rec *recRecorder) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ev := range rec.snapshot() {
		if ev.Action != "drive.seed" {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("drive.seed data: %v", err)
		}
		data["_outcome"] = ev.Outcome
		out = append(out, data)
	}
	return out
}

// wantOneRefusedRow asserts exactly one drive.seed row, refused with reason.
func wantOneRefusedRow(t *testing.T, rec *recRecorder, reason string) {
	t.Helper()
	rows := seedRows(t, rec)
	if len(rows) != 1 || rows[0]["_outcome"] != "failure" || rows[0]["reason"] != reason || rows[0]["outcome"] != "refused" {
		t.Fatalf("drive.seed rows = %v, want one refused with reason %s", rows, reason)
	}
}

// TestDriveSeed_IntoValidation: into is one relative path with no .., no leading slash, no control character and at
// most 255 bytes; anything else is 400 before the drive is read, and writes no row.
func TestDriveSeed_IntoValidation(t *testing.T) {
	st := newMemberDriveStore()
	srv, rec := memberDriveServer(st, &fillHolderRunner{}, "docker")
	id := fillDrive(t, srv)
	for _, into := range []string{"/a", "../a", "a/../b", "a//b", "a/", ".", "a/./b", "a\x01b", "a\x7fb", strings.Repeat("x", 256), "a\xffb"} {
		w := fillCall(srv, personCtx(aliceSub), id, fillBody("org/name", into))
		wantReason(t, w, http.StatusBadRequest, reasonDriveSeedIntoInvalid)
	}
	for _, into := range []string{"", "a", "a/b", "my dir/it's", strings.Repeat("x", 255)} {
		if !driveFillIntoValid(into) {
			t.Errorf("into %q refused, want accepted", into)
		}
	}
	if rows := seedRows(t, rec); len(rows) != 0 {
		t.Fatalf("a refused into wrote drive.seed rows: %v", rows)
	}
}

// TestDriveSeed_UnsupportedKindsRefuse: a deleting drive, an unsettled legacy one, a team drive, a disk drive, a
// host drive and a repository that is not https on github.com are 422 drive_seed_unsupported, one refused row each.
func TestDriveSeed_UnsupportedKindsRefuse(t *testing.T) {
	now := time.Now()
	drives := map[string]func(d *types.UserDrive){
		"deleting":   func(d *types.UserDrive) { d.DeleteStartedAt = &now },
		"unsettled":  func(d *types.UserDrive) { d.LegacyUnsettled = true },
		"team":       func(d *types.UserDrive) { d.Team = true },
		"mac disk":   func(d *types.UserDrive) { d.Backend = types.DriveBackendMacDisk },
		"host path":  func(d *types.UserDrive) { d.Backend = types.DriveBackendHostPath },
		"static pvc": func(d *types.UserDrive) { d.Backend = types.DriveBackendK8sPVCStatic },
	}
	for name, mutate := range drives {
		t.Run(name, func(t *testing.T) {
			st := newMemberDriveStore()
			srv, rec := memberDriveServer(st, &fillHolderRunner{}, "docker")
			id := fillDrive(t, srv)
			uid := uuid.MustParse(id)
			d := st.drives[uid]
			mutate(&d)
			st.drives[uid] = d
			body := wantReason(t, fillCall(srv, personCtx(aliceSub), id, fillBody("org/name", "")), http.StatusUnprocessableEntity, reasonDriveSeedUnsupported)
			if name == "deleting" && body.Error != "Notes is being deleted." {
				t.Errorf("sentence = %q, want the deleting one", body.Error)
			}
			wantOneRefusedRow(t, rec, reasonDriveSeedUnsupported)
		})
	}
	for _, repo := range []string{"https://dev.azure.com/org/proj/_git/repo", "git@github.com:org/name.git", "ssh://git@github.com/org/name.git", "https://gitlab.com/org/name", "http://github.com/org/name", "name"} {
		t.Run("repo "+repo, func(t *testing.T) {
			st := newMemberDriveStore()
			srv, rec := memberDriveServer(st, &fillHolderRunner{}, "docker")
			id := fillDrive(t, srv)
			wantReason(t, fillCall(srv, personCtx(aliceSub), id, fillBody(repo, "")), http.StatusUnprocessableEntity, reasonDriveSeedUnsupported)
			wantOneRefusedRow(t, rec, reasonDriveSeedUnsupported)
		})
	}
}

// TestDriveSeed_InUseRefuses: a sandbox that mounts the drive, and a run of the owner starting, are 409
// drive_seed_in_use, and neither reaches a create.
func TestDriveSeed_InUseRefuses(t *testing.T) {
	st := newMemberDriveStore()
	rn := &fillHolderRunner{holder: "sandbox-7"}
	srv, rec := memberDriveServer(st, rn, "docker")
	id := fillDrive(t, srv)
	body := wantReason(t, fillCall(srv, personCtx(aliceSub), id, fillBody("org/name", "")), http.StatusConflict, reasonDriveSeedInUse)
	if body.Error != "A run is using Notes. Fill it when no run has it." {
		t.Errorf("sentence = %q", body.Error)
	}
	wantOneRefusedRow(t, rec, reasonDriveSeedInUse)
	if len(rn.asked) != 1 || rn.asked[0].DriveID.String() != id {
		t.Errorf("holder asked about %v, want the drive %s", rn.asked, id)
	}

	st2 := newMemberDriveStore()
	srv2, rec2 := memberDriveServer(st2, &fillHolderRunner{}, "docker")
	id2 := fillDrive(t, srv2)
	st2.starting = map[string]bool{aliceSub: true}
	body = wantReason(t, fillCall(srv2, personCtx(aliceSub), id2, fillBody("org/name", "")), http.StatusConflict, reasonDriveSeedInUse)
	if !strings.HasPrefix(body.Error, "A run of yours is starting.") {
		t.Errorf("sentence = %q, want the starting one", body.Error)
	}
	wantOneRefusedRow(t, rec2, reasonDriveSeedInUse)
}

// TestDriveSeed_HolderUnknownRefuses503: a substrate with no holder read, and one whose read fails, are 503
// drive_seed_unavailable; the error text stays out of the body.
func TestDriveSeed_HolderUnknownRefuses503(t *testing.T) {
	for name, rn := range map[string]*fillHolderRunner{"read fails": {err: errors.New("daemon says secret-detail")}} {
		t.Run(name, func(t *testing.T) {
			st := newMemberDriveStore()
			srv, rec := memberDriveServer(st, rn, "docker")
			id := fillDrive(t, srv)
			w := fillCall(srv, personCtx(aliceSub), id, fillBody("org/name", ""))
			wantReason(t, w, http.StatusServiceUnavailable, reasonDriveSeedUnavailable)
			if strings.Contains(w.Body.String(), "secret-detail") {
				t.Errorf("the body carries the read error: %s", w.Body.String())
			}
			wantOneRefusedRow(t, rec, reasonDriveSeedUnavailable)
		})
	}
	t.Run("no capability", func(t *testing.T) {
		st := newMemberDriveStore()
		srv, rec := memberDriveServer(st, &memberDriveRunner{}, "docker")
		id := fillDrive(t, srv)
		wantReason(t, fillCall(srv, personCtx(aliceSub), id, fillBody("org/name", "")), http.StatusServiceUnavailable, reasonDriveSeedUnavailable)
		wantOneRefusedRow(t, rec, reasonDriveSeedUnavailable)
	})
}

// TestDriveSeed_NoAuditBeforeTheDrive: a caller that is not a person (403), a 429 and a body 400 write no
// drive.seed row; an id that is not the caller's writes authz.denied and no drive.seed row.
func TestDriveSeed_NoAuditBeforeTheDrive(t *testing.T) {
	st := newMemberDriveStore()
	st.site = memberDriveSite(20, 0)
	srv, rec := memberDriveServer(st, &fillHolderRunner{}, "docker")
	at := time.Now()
	srv.cfg.Now = func() time.Time { return at }
	id := fillDrive(t, srv)
	alice := personCtx(aliceSub)
	wantReason(t, fillCall(srv, context.Background(), id, fillBody("org/name", "")), http.StatusForbidden, reasonMemberDriveRequiresPerson)
	wantReason(t, fillCall(srv, alice, id, `{"workspace_id":"nope","repo":"org/name"}`), http.StatusBadRequest, reasonDriveSeedInvalid)
	wantReason(t, fillCall(srv, alice, id, `{"workspace_id":"`+fillWorkspace+`","repo":"org/name","ref":"-x"}`), http.StatusBadRequest, reasonDriveSeedInvalid)
	wantReason(t, fillCall(srv, alice, id, fmt.Sprintf(`{"workspace_id":%q}`, fillWorkspace)), http.StatusBadRequest, reasonDriveSeedInvalid)
	for i := 0; i < 80; i++ { // spend the person's tokens; the bucket refills with the clock, which stands still
		if w := fillCall(srv, alice, id, `{"workspace_id":"nope"}`); w.Code == http.StatusTooManyRequests {
			break
		}
	}
	wantReason(t, fillCall(srv, alice, id, fillBody("org/name", "")), http.StatusTooManyRequests, reasonMemberDriveRateLimited)
	if rows := seedRows(t, rec); len(rows) != 0 {
		t.Fatalf("drive.seed rows before the drive decision: %v", rows)
	}
	st2 := newMemberDriveStore()
	srv2, rec2 := memberDriveServer(st2, &fillHolderRunner{}, "docker")
	mine := fillDrive(t, srv2)
	wantReason(t, fillCall(srv2, personCtx(bobSub), uuid.NewString(), fillBody("org/name", "")), http.StatusNotFound, "drive_not_found")
	wantReason(t, fillCall(srv2, personCtx(bobSub), mine, fillBody("org/name", "")), http.StatusNotFound, "drive_not_found")
	if n := countAction(rec2, "authz.denied"); n != 2 || len(seedRows(t, rec2)) != 0 {
		t.Fatalf("a drive that is not the caller's wrote %d authz.denied rows and drive.seed rows %v, want 2 and none", n, seedRows(t, rec2))
	}
}

// runFillScript runs the fill's script on temp paths and returns its exit code and output.
func runFillScript(t *testing.T, src, target, into, mounts string) (int, string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	cmd := exec.Command("timeout", "20", "bash", "-c", driveFillScript(src, target, into, mounts))
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out)
	case errors.As(err, &ee):
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("run the script: %v", err)
	return -1, ""
}

// fillTree is a cloned source (with a .git folder and two files), an empty target and a mount table that lists it.
func fillTree(t *testing.T) (src, target, mounts string) {
	t.Helper()
	root := t.TempDir()
	src, target = filepath.Join(root, "seed-source"), filepath.Join(root, "drive")
	for _, dir := range []string{filepath.Join(src, ".git"), filepath.Join(src, "pkg"), target} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"README.md", "pkg/main.go"} {
		if err := os.WriteFile(filepath.Join(src, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return src, target, fillMounts(t, root, target)
}

func fillMounts(t *testing.T, root string, mounted ...string) string {
	t.Helper()
	var lines []string
	for _, m := range mounted {
		lines = append(lines, "vol "+m+" ext4 rw 0 0")
	}
	p := filepath.Join(root, "mounts")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// TestDriveSeedTask_RefusesNonEmpty: a file in the destination refuses (exit 3) and copies nothing; lost+found at
// the drive's root and the script's own lock are ignored, and under into lost+found refuses.
func TestDriveSeedTask_RefusesNonEmpty(t *testing.T) {
	src, target, mounts := fillTree(t)
	if err := os.WriteFile(filepath.Join(target, "mine.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := runFillScript(t, src, target, "", mounts); code != 3 || exists(filepath.Join(target, "README.md")) || exists(filepath.Join(target, ".wardyn-fill")) {
		t.Fatalf("a non-empty drive: exit %d, %s; want 3, nothing copied, the lock removed", code, out)
	}
	if err := os.Remove(filepath.Join(target, "mine.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(target, "lost+found"), 0o700); err != nil {
		t.Fatal(err)
	}
	if code, out := runFillScript(t, src, target, "", mounts); code != 0 || !exists(filepath.Join(target, "pkg", "main.go")) || exists(filepath.Join(target, ".wardyn-fill")) {
		t.Fatalf("lost+found at the root: exit %d, %s; want 0 and a copy", code, out)
	}
	src2, target2, mounts2 := fillTree(t)
	if err := os.MkdirAll(filepath.Join(target2, "sub", "lost+found"), 0o700); err != nil {
		t.Fatal(err)
	}
	if code, out := runFillScript(t, src2, target2, "sub", mounts2); code != 3 || exists(filepath.Join(target2, "sub", "README.md")) {
		t.Fatalf("lost+found under into: exit %d, %s; want 3", code, out)
	}
}

// TestDriveSeedTask_IntoStaysBelowTarget: with into the script writes only below target/into, and a drive that
// holds files elsewhere is untouched.
func TestDriveSeedTask_IntoStaysBelowTarget(t *testing.T) {
	src, target, mounts := fillTree(t)
	if err := os.WriteFile(filepath.Join(target, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := runFillScript(t, src, target, "a/b", mounts); code != 0 {
		t.Fatalf("exit %d, %s", code, out)
	}
	if !exists(filepath.Join(target, "a", "b", "pkg", "main.go")) || exists(filepath.Join(target, "README.md")) || exists(filepath.Join(target, "pkg")) {
		t.Fatalf("the copy is not confined to a/b")
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 2 {
		t.Fatalf("the target's top folder = %v (%v), want keep.txt and a", entries, err)
	}
}

// TestDriveSeedTask_NotAMountRefuses: a target that is not in the mount table is exit 4 and nothing is copied.
func TestDriveSeedTask_NotAMountRefuses(t *testing.T) {
	src, target, _ := fillTree(t)
	mounts := fillMounts(t, filepath.Dir(target), filepath.Join(filepath.Dir(target), "other"))
	if code, out := runFillScript(t, src, target, "", mounts); code != 4 {
		t.Fatalf("exit %d, %s; want 4", code, out)
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Fatalf("the target holds %v after a refusal", entries)
	}
	if code, out := runFillScript(t, filepath.Join(src, "pkg"), target, "", fillMounts(t, filepath.Dir(target), target)); code != 2 {
		t.Fatalf("a source with no .git: exit %d, %s; want 2", code, out)
	}
}

// TestDriveSeedTask_LockRefusesSecondFill: while another fill holds the lock the script exits 5, copies nothing
// and leaves the other fill's lock alone.
func TestDriveSeedTask_LockRefusesSecondFill(t *testing.T) {
	src, target, mounts := fillTree(t)
	lock := filepath.Join(target, ".wardyn-fill")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	if code, out := runFillScript(t, src, target, "", mounts); code != 5 || exists(filepath.Join(target, "README.md")) || !exists(lock) {
		t.Fatalf("a held lock: exit %d, %s; want 5, no copy and the lock kept", code, out)
	}
}

// TestDriveSeedTask_QuotesValues: a target and an into holding a quote and a command substitution, and an into with a space, reach
// the script as data: the files land at exactly that path and the command does not run.
func TestDriveSeedTask_QuotesValues(t *testing.T) {
	src, _, _ := fillTree(t)
	root := t.TempDir()
	target := filepath.Join(root, "it's$(touch${IFS}"+filepath.Join(root, "PWNED")+")dir") // no space: a mount point in /proc/mounts has none
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	into := "x y/it's `touch " + filepath.Join(root, "PWNED2") + "`"
	if code, out := runFillScript(t, src, target, into, fillMounts(t, root, target)); code != 0 {
		t.Fatalf("exit %d, %s", code, out)
	}
	if !exists(filepath.Join(target, into, "pkg", "main.go")) {
		t.Fatalf("the files are not at the exact path")
	}
	if exists(filepath.Join(root, "PWNED")) || exists(filepath.Join(root, "PWNED2")) {
		t.Fatalf("a value ran as a command")
	}
}
