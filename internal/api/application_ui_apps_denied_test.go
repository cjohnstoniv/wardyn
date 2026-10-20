// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An application whose declared app the profile's deny_ui_apps strips is refused at create, and preflight says the
// same (#3391). Before, the run was created without its app and only the clamp warning said so.
func TestApplicationRunRefusedWhenTheProfileStripsItsApp(t *testing.T) {
	// An application server runs a command, so it names no harness: the fixture has no sandbox image that carries one.
	const app = `"shape":"application","experience":"interactive",` + noRepos + `,"startup":{"kind":"command","command":"npm run dev"},` +
		`"inline_policy":{"ui_apps":[{"name":"web","port":3000}]}`
	const envShape = `"shape":"environment",` + interactiveOK + `,"inline_policy":{"ui_apps":[{"name":"web","port":3000}]}`

	for _, door := range contractDoors {
		t.Run(door.name, func(t *testing.T) {
			t.Run("a profile that strips ui apps refuses the application", func(t *testing.T) {
				srv, _, _ := govEscapeFixture(t, assignedStore(limitsProfile("no-ui", types.GovernanceLimits{DenyUIApps: true})))
				w := doSSO(t, srv, http.MethodPost, door.path, govSession(t, "sub-walled", []string{"eng"}, false), modeBody(app))
				if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonRunApplicationUIAppsDenied {
					t.Fatalf("%s = %d %s, want 422 %s", door.name, w.Code, w.Body.String(), reasonRunApplicationUIAppsDenied)
				}
				var body struct{ Error string }
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if body.Error != applicationUIAppsDeniedMsg {
					t.Fatalf("sentence = %q, want %q", body.Error, applicationUIAppsDeniedMsg)
				}
			})
			t.Run("a profile that allows them does not", func(t *testing.T) {
				srv, _, _ := govEscapeFixture(t, assignedStore(limitsProfile("open", types.GovernanceLimits{})))
				w := doSSO(t, srv, http.MethodPost, door.path, govSession(t, "sub-walled", []string{"eng"}, false), modeBody(app))
				if errorReason(w) == reasonRunApplicationUIAppsDenied {
					t.Fatalf("%s refused an application whose app is not stripped: %s", door.name, w.Body.String())
				}
				if door.name == "create" && w.Code != http.StatusCreated {
					t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
				}
			})
			t.Run("another shape keeps the strip-and-warn behaviour", func(t *testing.T) {
				srv, _, _ := govEscapeFixture(t, assignedStore(limitsProfile("no-ui", types.GovernanceLimits{DenyUIApps: true})))
				w := doSSO(t, srv, http.MethodPost, door.path, govSession(t, "sub-walled", []string{"eng"}, false), modeBody(envShape))
				if errorReason(w) == reasonRunApplicationUIAppsDenied {
					t.Fatalf("%s refused a non-application shape: %s", door.name, w.Body.String())
				}
			})
		})
	}
}

// A background application declares no app. Its ceiling may still name one, and under deny_ui_apps the default arm
// copies it and strips it; that run asked for no app, so it is created, as it was before the refusal existed.
func TestApplicationRunWithoutADeclaredAppIsNotRefusedForTheCeilingsApp(t *testing.T) {
	const bg = `"shape":"application","experience":"background","workload":{"kind":"command","command":"make"},` + noRepos
	for _, door := range contractDoors {
		t.Run(door.name, func(t *testing.T) {
			p := limitsProfile("no-ui", types.GovernanceLimits{DenyUIApps: true})
			p.Ceiling.UIApps = []types.UIApp{{Name: "admin", Port: 8080}}
			srv, _, _ := govEscapeFixture(t, assignedStore(p))
			w := doSSO(t, srv, http.MethodPost, door.path, govSession(t, "sub-walled", []string{"eng"}, false), modeBody(bg))
			want := http.StatusOK
			if door.name == "create" {
				want = http.StatusCreated
			}
			if w.Code != want {
				t.Fatalf("%s = %d %s, want %d", door.name, w.Code, w.Body.String(), want)
			}
		})
	}
}
