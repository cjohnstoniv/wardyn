// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A component may not set a variable that decides how programs in the sandbox
// start or reach the network. The refusal is the validator's, so it is the
// same at launch, at Review and at the policy preview, for a component written
// on the request, for a person's saved one and for an organisation's — as a
// config key and as the variable a secret is delivered in. A person is told
// which variable; for an organisation's component the content is the admin's.
func TestRunComponents_ReservedEnvironmentNamesAreRefusedAtEveryDoor(t *testing.T) {
	const personRowID = "6f0c1d2e-0000-4000-8000-0000000c0377"
	definitions := func(name string) map[string]types.ComponentDefinition {
		return map[string]types.ComponentDefinition{
			"config key": {Config: map[string]string{name: "v"}},
			"env variable": {Secrets: []types.ComponentSecret{{SecretName: compOwnSecret,
				Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryEnv, Var: name}}}},
		}
	}
	for _, name := range []string{"PATH", "HOME", "BASH_ENV", "NODE_OPTIONS", "PYTHONPATH", "GIT_SSH_COMMAND", "CLAUDE_CONFIG_DIR",
		"HTTPS_PROXY", "NO_PROXY", "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "LD_PRELOAD", "GIT_CONFIG_COUNT", "CLAUDE_CODE_ANYTHING"} {
		for where, def := range definitions(name) {
			for _, row := range []string{"inline", "a person's saved row", "an organisation's row"} {
				t.Run(name+"/"+where+"/"+row, func(t *testing.T) {
					f := newComponentFixture(t)
					var ref any
					switch row {
					case "inline":
						ref = map[string]any{"inline": def}
					case "a person's saved row":
						f.st.components = append(f.st.components, types.Component{ID: uuid.MustParse(personRowID), Owner: capSub, Name: "Mine", Version: 1, Definition: def})
						ref = map[string]any{"id": personRowID}
					default:
						ref = f.org(compOrgID, def)
					}
					body := componentBody(ref)
					var first string
					for i, door := range componentDoors {
						w := f.ask(t, door, body)
						if w.Code != http.StatusUnprocessableEntity {
							t.Fatalf("%s = %d %s, want 422", door, w.Code, w.Body.String())
						}
						got := decodeErrorBody(t, w)
						if got.Reason != reasonComponentDefinitionInvalid {
							t.Errorf("%s reason = %q, want %q", door, got.Reason, reasonComponentDefinitionInvalid)
						}
						if row == "an organisation's row" {
							if got.Error != "components[0]: this component has a definition that is not valid here. Ask your admin." {
								t.Errorf("%s = %q, want the sentence that says nothing of an organisation's component", door, got.Error)
							}
						} else if !strings.Contains(got.Error, `"`+name+`" is reserved`) {
							t.Errorf("%s = %q, want the variable named as reserved", door, got.Error)
						}
						if i == 0 {
							first = w.Body.String()
						} else if w.Body.String() != first {
							t.Errorf("%s answered %s, launch answered %s", door, w.Body.String(), first)
						}
					}
				})
			}
		}
	}

	// An ordinary variable beside them is admitted at all three doors.
	f := newComponentFixture(t)
	body := componentBody(map[string]any{"inline": types.ComponentDefinition{Config: map[string]string{"SERVICE_REGION": "eu", "MY_PATH": "/srv"}}})
	for _, door := range componentDoors {
		if w := f.ask(t, door, body); w.Code >= 300 {
			t.Errorf("%s = %d %s, want an ordinary config key admitted", door, w.Code, w.Body.String())
		}
	}
}
