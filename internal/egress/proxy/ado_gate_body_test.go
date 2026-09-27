// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
)

// Only the body routes are peeked. A 1 MiB wiki attachment and an npm publish
// classify on their path and stream through whole; the 256 KiB peek bound
// applies only where the body decides the capability.
func TestADOGate_LargeBodyOnAPathOnlyRouteStreamsThrough(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 1<<20)
	for _, tc := range []struct {
		name, host, path string
		want             adoscope.Capability
	}{
		{"wiki attachment", "dev.azure.com", "/acme/proj/_apis/wiki/wikis/w/attachments?name=a.bin", adoscope.CapWikiWrite},
		{"npm publish", "pkgs.dev.azure.com", "/acme/proj/_packaging/feed/npm/registry/pkg", adoscope.CapPackagingWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPut, tc.path, bytes.NewReader(big))
			r.Header.Set("Content-Length", strconv.Itoa(len(big)))
			grant := ADOGrant{Organization: "acme", Capabilities: []adoscope.Capability{tc.want}}
			if msg, held := adoCheck(r, tc.host, grant, adoRefProtected); msg != "" || held != nil {
				t.Fatalf("adoCheck = %q, %v; want forwarded under %s", msg, held, tc.want)
			}
			got, err := io.ReadAll(r.Body)
			if err != nil || !bytes.Equal(got, big) {
				t.Fatalf("the forwarded body is %d bytes (err %v), want the whole %d untouched", len(got), err, len(big))
			}
			// The capability is the one the path names: without it, refused.
			r = httptest.NewRequest(http.MethodPut, tc.path, bytes.NewReader(big))
			if msg, held := adoCheck(r, tc.host, ADOGrant{Organization: "acme"}, adoRefProtected); held == nil || held.Capability != tc.want {
				t.Fatalf("ungranted: adoCheck = %q, %v; want a hold naming %s", msg, held, tc.want)
			}
		})
	}
}

// End to end through the MITM: the 1 MiB attachment is forwarded under
// brokered:ado (the fake has no wiki route, so what answers is its 404).
func TestADOGate_LargeWikiAttachmentIsForwarded(t *testing.T) {
	h := newADOHarness(t, adoscope.CapRead, adoscope.CapWikiWrite)
	rec := h.do(t, http.MethodPut, "/acme/proj/_apis/wiki/wikis/w/attachments?name=a.bin", strings.Repeat("x", 1<<20), nil)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("a 1 MiB wiki attachment was refused: %s", rec.Body.String())
	}
	if log := h.log(); !strings.Contains(log, `"`+ruleSourceADO+`"`) || strings.Contains(log, ruleSourceADODenied) {
		t.Errorf("the attachment was not forwarded under %s: %s", ruleSourceADO, log)
	}
}

// A body route keeps the bound: a 300 KiB pull-request completion cannot be
// checked for bypassPolicy and is refused, whatever the run holds.
func TestADOGate_OversizedPRCompletionIsStillRefused(t *testing.T) {
	h := newADOHarness(t, adoscope.GrantableCapabilities()...)
	body := `{"status":"completed","description":"` + strings.Repeat("x", 300<<10) + `"}`
	h.mustRefuse(t, h.do(t, http.MethodPatch, "/acme/proj/_apis/git/repositories/app/pullrequests/5", body, nil), "too large")
}

// A duplicate key on a body route is still refused: which bypassPolicy the
// server acts on is not knowable.
func TestADOGate_DuplicateKeyOnABodyRouteIsRefused(t *testing.T) {
	h := newADOHarness(t, adoscope.GrantableCapabilities()...)
	h.mustRefuse(t, h.do(t, http.MethodPatch, "/acme/proj/_apis/git/repositories/app/pullrequests/5",
		`{"completionOptions":{"bypassPolicy":false,"bypassPolicy":true}}`, nil), "could not tell")
}

// One ref rule across both doors: a REST push whose ref walks out of the run
// namespace is refused, not read as a run-namespace code_write.
func TestADOGate_RESTRefTraversalOutOfTheRunNamespaceIsRefused(t *testing.T) {
	h := newADOHarness(t, adoscope.CapRead, adoscope.CapCodeWrite)
	body := `{"refUpdates":[{"name":"` + BranchNSPrefix(h.p.runID) + `../../main","oldObjectId":"` + zeroOID + `"}],"commits":[]}`
	h.mustRefuse(t, h.do(t, http.MethodPost, "/acme/proj/_apis/git/repositories/app/pushes?api-version=7.1", body, nil), "could not tell")
}

// The gate MUST withhold the body on its first ask. A body route classified
// with no peek and no declared length reads as an empty body — a chunked
// completion asking to bypass the branch policy would pass as a plain PR
// update.
func TestADOGate_ChunkedBypassCompletionIsPeeked(t *testing.T) {
	r := httptest.NewRequest(http.MethodPatch, "/acme/proj/_apis/git/repositories/app/pullrequests/5",
		strings.NewReader(`{"status":"completed","completionOptions":{"bypassPolicy":true}}`))
	r.ContentLength = -1
	r.Header.Del("Content-Length")
	grant := ADOGrant{Organization: "acme", Capabilities: []adoscope.Capability{adoscope.CapRead, adoscope.CapPR}}
	if msg, held := adoCheck(r, adoHost, grant, adoRefProtected); held == nil || held.Capability != adoscope.CapPolicyBypass {
		t.Fatalf("adoCheck = %q, %v; want a hold naming %s", msg, held, adoscope.CapPolicyBypass)
	}
}
