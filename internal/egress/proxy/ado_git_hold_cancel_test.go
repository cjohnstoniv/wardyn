// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestADOGitHold_ClientHangUpMidHoldLeavesNoDeniedRow: git's twin of
// TestADOHold_ClientHangUpMidHoldLeavesNoDeniedRow. A push held for code_write
// whose client disconnects mid-hold was never refused, so the broker emits no
// brokered:ado-git:denied decision and writes no body.
func TestADOGitHold_ClientHangUpMidHoldLeavesNoDeniedRow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fastPolls(t, 5*time.Millisecond)
	p, sink, _ := newEntraProxy(t, types.RunPolicySpec{}, "127.0.0.1:1", adoscope.CapCodeRead)
	tok := &tokenSource{}
	tok.Set("run-token")
	cp := newCapControlPlane(t)
	inj := p.inject
	inj.base, inj.token, inj.client = cp.srv.URL, tok, cp.srv.Client()
	inj.reauth, inj.approvals = newReauthCoordinator(), cancelOnReadApproval{cancel}
	t.Cleanup(inj.reauth.stop)

	body := recordedPush(t, BranchNSPrefix(p.runID)+"work", map[string]string{"README.md": "hello\n"})
	req := mustLocalReq(t, http.MethodPost, "/wardyn/git/dev.azure.com/acme/proj/_git/app/git-receive-pack", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req.WithContext(ctx))

	if ctx.Err() == nil {
		t.Fatal("the push was never held: the approval was not read")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("a hung-up client got a body: %q", rec.Body.String())
	}
	if log := sink.String(); strings.Contains(log, ruleSourceADOGitDenied) {
		t.Errorf("a hung-up client has a %s row:\n%s", ruleSourceADOGitDenied, log)
	}
}
