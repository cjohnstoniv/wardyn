// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// watermarkRunStore is runTypeStore with the site config a test sets.
type watermarkRunStore struct {
	runTypeStore
	sc  types.SiteConfig
	err error
}

func (s watermarkRunStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.sc, s.err
}

// GET /runs/{id} carries desktop_watermark for a run that can show a desktop, with the caller's email, else their
// principal, the run id and the organisation's rule (required when the site config cannot be read). A container run
// has no such key.
func TestGetRunDesktopWatermark(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    types.MachineKind
		email   string
		sc      types.SiteConfig
		err     error
		want    bool
		who     string
		require bool
	}{
		{name: "a Linux VM run, no rule", kind: types.MachineLinuxVM, email: "pat@corp.example", want: true, who: "pat@corp.example"},
		{name: "a macOS VM run, required", kind: types.MachineMacOSVM, email: "pat@corp.example", want: true, who: "pat@corp.example",
			sc: types.SiteConfig{DesktopWatermark: &types.SiteDesktopWatermark{Required: true}}, require: true},
		{name: "a caller with no email is named by principal", kind: types.MachineLinuxVM, want: true, who: "sub-pm"},
		{name: "an unreadable site config reads as required", kind: types.MachineLinuxVM, email: "pat@corp.example", err: errors.New("db down"),
			want: true, who: "pat@corp.example", require: true},
		{name: "a container run carries none", kind: types.MachineContainer},
		{name: "a run with no machine kind carries none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := watermarkRunStore{runTypeStore: runTypeStore{newUIMemStore()}, sc: tc.sc, err: tc.err}
			cfg := baseTestConfig(newHarness(t), st)
			cfg.OIDC = newAccessAuth(t, nil, "", nil, nil)
			srv := New(cfg)
			owner := accessSessionOfType(t, "sub-pm", tc.email, oidc.RoleUser, "portfolio-manager", []string{})
			run := types.AgentRun{ID: uuid.New(), CreatedBy: "sub-pm", State: types.RunRunning, MachineKind: tc.kind}
			st.putRun(run)

			w := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+run.ID.String(), owner, "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET run = %d; body=%s", w.Code, w.Body.String())
			}
			var one map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &one); err != nil {
				t.Fatal(err)
			}
			got, has := one["desktop_watermark"].(map[string]any)
			if has != tc.want {
				t.Fatalf("desktop_watermark present = %v, want %v; body=%s", has, tc.want, w.Body.String())
			}
			if !tc.want {
				if _, key := one["desktop_watermark"]; key {
					t.Errorf("desktop_watermark key is in the body of a run with no desktop: %s", w.Body.String())
				}
				return
			}
			if got["who"] != tc.who || got["run"] != run.ID.String() || got["required"] != tc.require {
				t.Errorf("desktop_watermark = %v, want who %s, run %s, required %v", got, tc.who, run.ID, tc.require)
			}
		})
	}
}
