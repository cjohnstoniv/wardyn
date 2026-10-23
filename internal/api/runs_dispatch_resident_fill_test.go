// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// With WARDYN_RUNNER_SECRET_PLACEHOLDERS off, or on a remote run, the resident
// placers return their input as it came and touch nothing: the variable, the
// file, the injections, the interception hosts and the values to deliver are
// equal to what dispatch had before.
func TestPlaceResidentSecrets_OffOrRemoteTouchesNothing(t *testing.T) {
	env := types.CredentialGrant{ID: uuid.New(), Delivery: types.GrantDeliveryResident, Spec: types.GrantSpec{Kind: types.GrantEnvSecret,
		Scope: json.RawMessage(`{"name":"RES_TOKEN","secret_name":"op-secret","host":"api.corp.test"}`)}}
	file := types.CredentialGrant{ID: uuid.New(), Delivery: types.GrantDeliveryResident, Spec: types.GrantSpec{Kind: types.GrantFileSecret,
		Scope: json.RawMessage(`{"file":"res.key","secret_name":"op-secret","host":"api.corp.test"}`)}}
	for _, tc := range []struct {
		name      string
		on        bool
		placement types.Placement
	}{
		{"off, local", false, types.PlacementLocal},
		{"on, remote", true, types.PlacementRemote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{cfg: Config{RunnerSecretPlaceholders: tc.on}}
			run := types.AgentRun{ID: uuid.New(), Placement: tc.placement}
			resident := []residentValue{
				{grant: env, class: placement.ClassEnvSecret, value: []byte("env-value")},
				{grant: file, class: placement.ClassFileSecret, value: []byte("file-value")},
			}
			want := slices.Clone(resident)
			sandboxEnv := map[string]string{"RES_TOKEN": placement.ResidentPlaceholder(env.ID)}
			wantEnv := maps.Clone(sandboxEnv)
			var injections []runner.InjectionGrant
			policy := types.RunPolicySpec{AllowedDomains: []string{"api.corp.test"}, EligibleGrants: []types.GrantSpec{env.Spec, file.Spec}}
			got := s.placeResidentEnvSecrets(t.Context(), run, policy, dispatchParams{}, sandboxEnv, &injections, resident)
			if !reflect.DeepEqual(got, want) || injections != nil || !maps.Equal(sandboxEnv, wantEnv) {
				t.Fatalf("env placer changed dispatch: values %+v injections %+v env %v", got, injections, sandboxEnv)
			}
			files := []runner.ManagedFile{{Path: runner.ComponentSecretDir + "/res.key", AgentOwned: true, Content: []byte(placement.ResidentPlaceholder(file.ID))}}
			wantFiles := slices.Clone(files)
			spec := runner.SandboxSpec{ProxyConfig: runner.ProxyConfig{Policy: policy}}
			wantSpec := spec
			if got := s.placeResidentFileSecrets(t.Context(), run, files, &spec, resident); !reflect.DeepEqual(got, want) ||
				!reflect.DeepEqual(files, wantFiles) || !reflect.DeepEqual(spec, wantSpec) {
				t.Fatalf("file placer changed dispatch: values %+v files %+v spec %+v", got, files, spec.ProxyConfig)
			}
			if string(resident[0].value) != "env-value" || string(resident[1].value) != "file-value" {
				t.Fatal("a value was cleared")
			}
		})
	}
}
