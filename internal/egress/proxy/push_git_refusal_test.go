// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestReceivePackHead_ReadsRefsAndCaps(t *testing.T) {
	oid := strings.Repeat("a", 40)
	body := pktLine(zeroOID+" "+oid+" refs/heads/a\x00report-status side-band-64k\n") +
		pktLine(zeroOID+" "+oid+" refs/heads/b\n") +
		pktLine("shallow "+oid+"\n") + "0000" + "PACK..."
	got := receivePackHead([]byte(body))
	if want := []string{"refs/heads/a", "refs/heads/b"}; !reflect.DeepEqual(got.refs, want) {
		t.Errorf("refs = %v, want %v", got.refs, want)
	}
	if want := []string{"report-status", "side-band-64k"}; !reflect.DeepEqual(got.caps, want) {
		t.Errorf("caps = %v, want %v", got.caps, want)
	}
	for _, trunc := range []string{"00", "0040" + zeroOID, "zzzz", ""} {
		if h := receivePackHead([]byte(trunc)); h == nil || len(h.refs) != 0 {
			t.Errorf("receivePackHead(%q) = %+v, want no refs", trunc, h)
		}
	}
}

func TestReadADOGitPush_DropsRefsWhenTheSectionDoesNotRead(t *testing.T) {
	oid := strings.Repeat("a", 40)
	body := pktLine(zeroOID+" "+oid+" refs/heads/a\x00report-status\n") + "zzzz"
	r := httptest.NewRequest(http.MethodPost, "/git-receive-pack", strings.NewReader(body))
	_, push, msg := readADOGitPush(r)
	if msg == "" {
		t.Fatal("msg is empty, want a refusal for a malformed section")
	}
	if push == nil || push.refs != nil {
		t.Errorf("push = %+v, want nil refs", push)
	}
	if want := []string{"report-status"}; push == nil || !reflect.DeepEqual(push.caps, want) {
		t.Errorf("caps = %+v, want %v", push, want)
	}
}

// wantRejectedRef asserts git printed a rejected ref carrying want, never a bare 403, and that nothing landed.
func wantRejectedRef(t *testing.T, r *forgeRun, out string, err error, want, row string) {
	t.Helper()
	mustBeGitRefusal(t, out, err, want)
	if !strings.Contains(out, "remote rejected") {
		t.Errorf("git output has no rejected ref:\n%s", out)
	}
	if strings.Contains(out, "HTTP 403") {
		t.Errorf("git output still shows a bare HTTP 403:\n%s", out)
	}
	if r.landed(t) {
		t.Error("the refused push landed on the forge")
	}
	if !strings.Contains(r.decisions.String(), row) {
		t.Errorf("decision log = %q, want a %s row", r.decisions.String(), row)
	}
}

func TestPushHold_GitPrintsTheUnattendedRefusal_AppLane(t *testing.T) {
	r := newForgeRunSpec(t, "", reviewSpec(1, []string{"src/**"}), ordinaryRepo, []string{"--depth", "1"})
	r.p.pushHolds.unattended = true
	r.editMain(t, "unattended")
	out, err := r.push()
	wantRejectedRef(t, r, out, err, "this run is unattended", ruleSourceGitPushHeldUnattended)
	if !strings.Contains(out, "run the task interactively so an admin can approve it") {
		t.Errorf("git output does not name the remedy:\n%s", out)
	}
}

func TestPushHold_GitPrintsTheUnattendedRefusal_TokenLane(t *testing.T) {
	r := newForgeRunSpec(t, "gitlab.com", reviewSpec(1, []string{"src/**"}), ordinaryRepo, []string{"--depth", "1"})
	r.p.pushHolds.unattended = true
	r.editMain(t, "unattended")
	out, err := r.push()
	wantRejectedRef(t, r, out, err, "this run is unattended", ruleSourceGitPushHeldUnattended)
	if !strings.Contains(out, "run the task interactively so an admin can approve it") {
		t.Errorf("git output does not name the remedy:\n%s", out)
	}
}

// A refusal on a resolved match stays a 403: only unattended and unresolved refusals are rejected refs.
func TestPushHold_AnAdminDenyStaysA403_AppLane(t *testing.T) {
	p, _, _, _, _ := newAppLaneHold(t, reviewSpec(5, []string{".github/workflows/**"}), types.ApprovalDenied)
	rec := postPush(t, p, string(recordedPush(t, BranchNSPrefix(p.runID)+"work", workflowPush)))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "an admin denied this push") {
		t.Fatalf("status = %d body %q, want a 403 naming the admin's deny", rec.Code, rec.Body.String())
	}
}

func TestPushHold_GitPrintsAnUnresolvedRefusal_TokenLane(t *testing.T) {
	r := newForgeRunSpec(t, "gitlab.com", reviewSpec(1, []string{".github/workflows/**"}), ordinaryRepo, []string{"--depth", "1"})
	r.editMain(t, "unresolved")
	out, err := r.push()
	wantRejectedRef(t, r, out, err, "could not be worked out on this lane", ruleSourceGitPushHeld)
}
