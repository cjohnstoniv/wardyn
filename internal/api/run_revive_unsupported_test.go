// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// k8sLikeRunner answers CanReplaceProxy the way the orchestrator does for a
// Kubernetes sandbox, whose substrate implements no runner.ProxyReviver.
type k8sLikeRunner struct{ *reviveRunner }

func (k8sLikeRunner) CanReplaceProxy(context.Context, string) error {
	return runner.ErrReviveUnsupported
}

// TestReviveRun_UnsupportedSubstrateNamesItsReason (#1342): a live run on a
// substrate that cannot replace its proxy is refused with 409 and
// revive_unsupported, on the single-run revive and in the bulk restart's
// per-run result alike, whether the runner says so per ref (the
// orchestrator's CanReplaceProxy) or is no ProxyReviver at all. Nothing is
// replaced and the run stays live, not lost.
func TestReviveRun_UnsupportedSubstrateNamesItsReason(t *testing.T) {
	runners := map[string]func(f *reviveFixture) runner.Runner{
		"CanReplaceProxy refuses the ref": func(f *reviveFixture) runner.Runner { return k8sLikeRunner{f.rr} },
		"the runner is no ProxyReviver":   func(f *reviveFixture) runner.Runner { return f.lr },
	}
	for name, pick := range runners {
		t.Run(name, func(t *testing.T) {
			f := newReviveFixture(t)
			f.st.run.LostAt, f.st.run.LostReason = nil, ""
			f.srv.cfg.Runner = pick(f)

			w := do(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/revive", adminToken, "")
			var body errorBody
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusConflict || body.Reason != "revive_unsupported" || body.Error != runner.ErrReviveUnsupported.Error() {
				t.Errorf("revive: %d %+v; want 409 revive_unsupported with runner.ErrReviveUnsupported's text", w.Code, body)
			}

			w = do(t, f.srv, http.MethodPost, "/api/v1/admin/runs/restart", adminToken, `{"run_ids":["`+f.run.ID.String()+`"]}`)
			var out struct {
				Results []adminRestartResult `json:"results"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusOK || len(out.Results) != 1 {
				t.Fatalf("restart: %d %s; want 200 with one result", w.Code, w.Body.String())
			}
			if r := out.Results[0]; r.OK || r.LostAgain || r.Reason != "revive_unsupported" || r.Error != runner.ErrReviveUnsupported.Error() {
				t.Errorf("restart result %+v; want refused with revive_unsupported and not lost again", r)
			}

			if len(f.rr.replaced) != 0 {
				t.Error("a refused revive replaced the proxy")
			}
			if lostAt, _ := f.st.lost(); lostAt != nil {
				t.Error("a refused revive of a live run marked it lost")
			}
		})
	}
}
