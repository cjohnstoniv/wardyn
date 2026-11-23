package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The admin bulk restart refuses a Windows VM run in its result, and replaces no proxy.
func TestWindowsVMAdminRestartRefused(t *testing.T) {
	f := newReviveFixture(t)
	f.st.run.MachineKind = types.MachineWindowsVM
	f.run = f.st.run
	res := f.restart(t)
	if res.OK || res.Reason != reasonWindowsVMFeatureRefused || res.Error != windowsDeskOnlyMsg("revive") {
		t.Errorf("restart result = %+v, want refused %s", res, reasonWindowsVMFeatureRefused)
	}
	if len(f.rr.replaced) != 0 {
		t.Errorf("ReplaceProxy calls = %d, want none", len(f.rr.replaced))
	}
}

// A live egress edit that would revive a Windows VM run's proxy is refused, and replaces no proxy.
func TestWindowsVMEgressEditRefused(t *testing.T) {
	f := newEgressEditFixture(t)
	f.st.run.MachineKind = types.MachineWindowsVM
	f.run = f.st.run
	w := f.editEgress(t, `{"deny_hosts":["evil.example"]}`)
	if w.Code != http.StatusConflict || errorReasonOf(w.Body.String()) != reasonWindowsVMFeatureRefused ||
		!strings.Contains(w.Body.String(), windowsDeskOnlyMsg("revive")) {
		t.Errorf("egress edit = %d %s, want 409 %s", w.Code, w.Body, reasonWindowsVMFeatureRefused)
	}
	if len(f.rr.replaced) != 0 {
		t.Errorf("ReplaceProxy calls = %d, want none", len(f.rr.replaced))
	}
}

// A token launch is refused, even one that carries the person's own identity: a person's API token, the CLI's
// sign-in token, a token delegated to a portal, and an issuer-portal grant. Only an interactive sign-in starts a Windows VM run.
func TestWindowsVMTokenLaunchRefused(t *testing.T) {
	srv, ws, _ := winvmServer(t, true)
	for name, ctx := range map[string]context.Context{
		"a person's API token":    withAPITokenID(memberContext(), uuid.New()),
		"the CLI's sign-in token": context.WithValue(withAPITokenID(memberContext(), uuid.New()), apiTokenKindCtxKey{}, types.APITokenKindCLIAccess),
		"a delegated token":       audit.WithDelegation(memberContext(), types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New(), Actor: &types.DelegationActor{Sub: "agent", Iss: "https://portal.example"}}),
		"an issuer-portal grant":  audit.WithDelegation(memberContext(), types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()}),
	} {
		_, rf := srv.windowsImageDecision(winvmHTTP(ctx), winvmReq(), types.RunPolicySpec{})
		wantWinRefusal(t, name, rf, http.StatusForbidden, reasonWindowsVMFeatureRefused, "")
		if rf != nil && rf.body.Error != windowsTokenMsg {
			t.Errorf("%s: %q, want the token sentence", name, rf.body.Error)
		}
	}
	if len(ws.checks) != 0 {
		t.Fatalf("a token launch reached the seat check: %v", ws.checks)
	}
	if _, rf := srv.windowsImageDecision(winvmHTTP(memberContext()), winvmReq(), types.RunPolicySpec{}); rf != nil {
		t.Errorf("an interactive sign-in refused: %s", rf.body.Error)
	}
}
