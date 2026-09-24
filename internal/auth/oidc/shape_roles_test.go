// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"bufio"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestShippedShapeRoleDerivation proves, for every SSO deployment shape Wardyn
// ships, which role each test identity's sign-in derives — read from the
// SHIPPED config file for that shape, not a hand copy of it, so a role-map edit
// in any of them reddens here before a walk does.
//
// The kind overlay's identities are read from its Dex config too, and must
// match the expectation table one-for-one: a Dex user with no expected role, or
// an expected role with no Dex user, is the drift between the walk's cast and
// this proof. Dex's password DB (v2.40) emits no groups claim, so those
// identities are keyed by email; the group and App-Role arms are exercised on
// the m′ profile, whose map is keyed on App Roles.
//
// The two chart renders (SSO + admin token, and auth.ssoOnly) share one
// values.yaml, so they share one derivation; what the API then allows each role
// on each render is internal/api's TestSSOShapeRoleMatrix. The two compose
// harness shapes are the shipped env file with test/sso-roles/<shape>.env
// layered on, merged exactly as scripts/compose-sso-roles.sh merges them.
func TestShippedShapeRoleDerivation(t *testing.T) {
	const deny = "" // login refused: no_role

	type id struct {
		email         string
		roles, groups []string
		want          string
	}
	shapes := []struct {
		name string
		env  map[string]string
		ids  []id
		dex  string // Dex config whose staticPasswords must equal ids' emails
	}{
		{
			name: "kind chart overlay (SSO + admin token, and auth.ssoOnly)",
			env:  helmEnv(t, "deploy/kind/sso/values.yaml"),
			dex:  "deploy/kind/sso/dex.yaml",
			ids: []id{
				{email: "admin@wardyn.local", want: RoleAdmin},
				{email: "operator@wardyn.local", want: RoleAdmin}, // operator allowlist only
				{email: "secadmin@wardyn.local", want: RoleSecurityAdmin},
				{email: "member@wardyn.local", want: RoleUser},
				{email: "member2@wardyn.local", want: RoleUser},
				{email: "stranger@wardyn.local", want: deny},
			},
		},
		{
			name: "desktop member mode (m′)",
			env:  envFile(t, "deploy/desktop/wardyn.env.m-prime.example"),
			ids: []id{
				{email: "dev@example.com", roles: []string{"Wardyn.Admin"}, want: RoleAdmin},
				{email: "dev@example.com", roles: []string{"Wardyn.Member"}, want: RoleUser},
				{email: "dev@example.com", groups: []string{"Wardyn.Member"}, want: RoleUser},
				{email: "dev@example.com", roles: []string{"Wardyn.Member", "Wardyn.Admin"}, want: RoleAdmin},
				{email: "platform-team@example.com", want: RoleAdmin}, // operator allowlist
				{email: "dev@example.com", want: RoleUser},            // default role: the point of m′
			},
		},
		{
			name: "m′ harness (m-prime example + test/sso-roles/mprime.env)",
			env:  overlay(envFile(t, "deploy/desktop/wardyn.env.m-prime.example"), envFile(t, "test/sso-roles/mprime.env")),
			dex:  "test/sso-roles/dex.yaml",
			ids: []id{
				{email: "admin@wardyn.local", want: RoleAdmin},
				{email: "operator@wardyn.local", want: RoleAdmin},
				{email: "secadmin@wardyn.local", want: RoleSecurityAdmin},
				{email: "member@wardyn.local", want: RoleUser},
				{email: "member2@wardyn.local", want: RoleUser},  // default role
				{email: "stranger@wardyn.local", want: RoleUser}, // default role, by design
			},
		},
		{
			name: "compose harness (.env.example + test/sso-roles/compose-sso.env)",
			env:  overlay(envFile(t, "deploy/compose/.env.example"), envFile(t, "test/sso-roles/compose-sso.env")),
			dex:  "test/sso-roles/dex.yaml",
			ids: []id{
				{email: "admin@wardyn.local", want: RoleAdmin},
				{email: "operator@wardyn.local", want: RoleAdmin},
				{email: "secadmin@wardyn.local", want: RoleSecurityAdmin},
				{email: "member@wardyn.local", want: RoleUser},
				{email: "member2@wardyn.local", want: RoleUser},
				{email: "stranger@wardyn.local", want: deny},
			},
		},
		{
			name: "compose sso profile (.env.example)",
			env:  envFile(t, "deploy/compose/.env.example"),
			ids: []id{
				{email: "demo@wardyn.local", want: RoleAdmin},
				{email: "member@wardyn.local", want: RoleUser},
				{email: "stranger@wardyn.local", want: deny},
			},
		},
	}

	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			roleMap, err := ParseRoleMap(sh.env["WARDYN_OIDC_ROLE_MAP"])
			if err != nil || len(roleMap) == 0 {
				t.Fatalf("WARDYN_OIDC_ROLE_MAP %q: map=%v err=%v — every shipped SSO shape carries one", sh.env["WARDYN_OIDC_ROLE_MAP"], roleMap, err)
			}
			var admins []string
			for _, e := range strings.Split(sh.env["WARDYN_OIDC_OPERATOR_EMAILS"], ",") {
				if e = strings.TrimSpace(e); e != "" {
					admins = append(admins, e)
				}
			}
			merged, _ := mergeRoleMaps(roleMap, admins, nil)
			for _, c := range sh.ids {
				role, _, ok := deriveRole(c.roles, c.groups, c.email, merged, admins, sh.env["WARDYN_OIDC_DEFAULT_ROLE"])
				if !ok {
					role = deny
				}
				if role != c.want {
					t.Errorf("%s roles=%v groups=%v: derived %q (ok=%v), want %q", c.email, c.roles, c.groups, role, ok, c.want)
				}
			}
			if sh.dex == "" {
				return
			}
			var want []string
			for _, c := range sh.ids {
				want = append(want, c.email)
			}
			if got := dexEmails(t, sh.dex); !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
				t.Errorf("%s users = %v, want exactly the identities above %v", sh.dex, got, want)
			}
		})
	}
}

func repoFile(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// envFile reads the uncommented KEY=VALUE lines of an env file.
func envFile(t *testing.T, rel string) map[string]string {
	t.Helper()
	env := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(repoFile(t, rel))))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
			env[k] = v
		}
	}
	return env
}

// overlay returns base with every key of top replacing it.
func overlay(base, top map[string]string) map[string]string {
	maps.Copy(base, top)
	return base
}

func helmEnv(t *testing.T, rel string) map[string]string {
	t.Helper()
	var v struct {
		Env map[string]string `yaml:"env"`
	}
	if err := yaml.Unmarshal(repoFile(t, rel), &v); err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return v.Env
}

// dexEmails reads the staticPasswords of a Dex config — a plain one, or the
// first ConfigMap in a manifest carrying one under data."config.yaml".
func dexEmails(t *testing.T, rel string) []string {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(string(repoFile(t, rel))))
	for {
		var doc struct {
			Kind            string            `yaml:"kind"`
			Data            map[string]string `yaml:"data"`
			StaticPasswords []struct {
				Email string `yaml:"email"`
			} `yaml:"staticPasswords"`
		}
		if err := dec.Decode(&doc); err != nil {
			t.Fatalf("%s: no Dex config with staticPasswords: %v", rel, err)
		}
		if doc.Kind == "ConfigMap" && doc.Data["config.yaml"] != "" {
			if err := yaml.Unmarshal([]byte(doc.Data["config.yaml"]), &doc); err != nil {
				t.Fatalf("%s config.yaml: %v", rel, err)
			}
		}
		if len(doc.StaticPasswords) == 0 {
			continue
		}
		var out []string
		for _, p := range doc.StaticPasswords {
			out = append(out, p.Email)
		}
		return out
	}
}
