package api

import (
	"net/http"
	"strings"
	"testing"

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
