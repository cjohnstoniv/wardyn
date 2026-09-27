// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// featureMintStore backs both mint doors: the token double plus the SSH key
// double's two methods handleAddSSHKey reaches.
type featureMintStore struct {
	*tokenMemStore
	ssh *sshMemStore
}

func (s featureMintStore) AddSSHKey(ctx context.Context, k types.SSHPublicKey) (types.SSHPublicKey, error) {
	return s.ssh.AddSSHKey(ctx, k)
}

func (s featureMintStore) ListSSHKeysByPrincipal(ctx context.Context, principal string) ([]types.SSHPublicKey, error) {
	return s.ssh.ListSSHKeysByPrincipal(ctx, principal)
}

// featureFixture wires a Server whose capability reads answer from cs and whose
// mint doors write to in-memory doubles the test can count.
func featureFixture(t *testing.T, cs *capStore) (*Server, featureMintStore) {
	t.Helper()
	h := newHarness(t)
	inner := featureMintStore{tokenMemStore: newTokenMemStore(), ssh: newSSHMemStore()}
	cs.Store = inner
	if cs.userTypes == nil {
		cs.userTypes = utKnown
	}
	cfg := baseTestConfig(h, cs)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Audit = &recRecorder{}
	return New(cfg), inner
}

const featureTestKey = `{"name":"laptop","public_key":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBl3jvXfmZbBd3q5aLKZTv3rIcvKlfz2eYQpuYSGfCPT pm@laptop"}`

// featureDoor is one mint door: where it is, what it posts, the value it asks
// the resolver about, and how many rows it has written.
type featureDoor struct {
	value, path, body, target, refusal string
	written                            func(featureMintStore) int
}

var featureDoors = []featureDoor{
	{featureSSHKey, "/api/v1/me/ssh-keys", featureTestKey, "me.ssh_keys", sshKeyFeatureRefusal,
		func(st featureMintStore) int { return len(st.ssh.keys) }},
	{featureAPIToken, "/api/v1/me/tokens", `{"name":"ci"}`, "me.tokens", apiTokenFeatureRefusal,
		func(st featureMintStore) int { return len(st.byID) }},
}

// TestCapabilityFeatureKind: `feature` narrows whether a person may add an SSH
// key or mint an API token at all, one check at each mint door. Every subtest
// asks both doors, and a refusal must write nothing.
func TestCapabilityFeatureKind(t *testing.T) {
	allow, deny := types.CapabilityAllow, types.CapabilityDeny
	enforced := func() map[string]bool { return map[string]bool{capFeature: true} }
	pm := func(t *testing.T) *http.Cookie {
		return ssoSessionOfType(t, "sub-pm", "pm@corp.example", oidc.RoleUser, utPM)
	}

	// try posts to one door on a fresh fixture. reasons are "reason@target"
	// for every authz.denied row the request wrote.
	try := func(t *testing.T, d featureDoor, cs *capStore, cookie *http.Cookie) (code int, body string, written int, reasons []string) {
		t.Helper()
		srv, st := featureFixture(t, cs)
		w := doSSO(t, srv, http.MethodPost, d.path, cookie, d.body)
		for _, ev := range srv.cfg.Audit.(*recRecorder).snapshot() {
			if ev.Action == "authz.denied" {
				reasons = append(reasons, "@"+ev.Target)
			}
		}
		for i, r := range auditReasons(t, srv, "authz.denied") {
			reasons[i] = r + reasons[i]
		}
		return w.Code, w.Body.String(), d.written(st), reasons
	}
	wantRefused := func(t *testing.T, d featureDoor, code int, body string, written int, reasons []string) {
		t.Helper()
		if code != http.StatusForbidden {
			t.Fatalf("%s: code = %d, want 403: %s", d.path, code, body)
		}
		if !strings.Contains(body, d.refusal) {
			t.Errorf("%s: body = %s, want %q", d.path, body, d.refusal)
		}
		if written != 0 {
			t.Errorf("%s: a refused mint wrote %d row(s)", d.path, written)
		}
		if want := []string{"capability_feature@" + d.target}; !slices.Equal(reasons, want) {
			t.Errorf("%s: authz.denied = %v, want %v", d.path, reasons, want)
		}
	}
	wantMinted := func(t *testing.T, d featureDoor, code int, body string, written int, reasons []string) {
		t.Helper()
		if code != http.StatusCreated || written != 1 {
			t.Fatalf("%s: code = %d, rows = %d, want 201 and one row: %s", d.path, code, written, body)
		}
		if len(reasons) != 0 {
			t.Errorf("%s: authz.denied reasons = %v, want no capability_feature", d.path, reasons)
		}
	}

	for _, d := range featureDoors {
		other := featureAPIToken
		if d.value == featureAPIToken {
			other = featureSSHKey
		}
		t.Run(d.value, func(t *testing.T) {
			t.Run("unenforced, no grant: minted (upgrade day is unchanged)", func(t *testing.T) {
				c, b, n, r := try(t, d, &capStore{}, pm(t))
				wantMinted(t, d, c, b, n, r)
			})
			t.Run("enforced, no grant: refused", func(t *testing.T) {
				c, b, n, r := try(t, d, &capStore{enf: enforced()}, pm(t))
				wantRefused(t, d, c, b, n, r)
			})
			t.Run("enforced, an allow on the caller's user type: minted; on another type: refused", func(t *testing.T) {
				rows := []types.CapabilityGrant{grant(types.CapabilitySubjectUserType, utPM, capFeature, d.value, allow)}
				c, b, n, r := try(t, d, &capStore{enf: enforced(), grants: rows}, pm(t))
				wantMinted(t, d, c, b, n, r)
				dev := ssoSessionOfType(t, "sub-dev", "dev@corp.example", oidc.RoleUser, utDev)
				c, b, n, r = try(t, d, &capStore{enf: enforced(), grants: rows}, dev)
				wantRefused(t, d, c, b, n, r)
			})
			t.Run("unenforced, a deny on the caller's user type: refused (the type is turned off)", func(t *testing.T) {
				rows := []types.CapabilityGrant{grant(types.CapabilitySubjectUserType, utPM, capFeature, d.value, deny)}
				c, b, n, r := try(t, d, &capStore{grants: rows}, pm(t))
				wantRefused(t, d, c, b, n, r)
			})
			t.Run("a user allow does not lift a type deny", func(t *testing.T) {
				rows := []types.CapabilityGrant{
					grant(types.CapabilitySubjectUserType, utPM, capFeature, capWildcard, deny),
					grant(types.CapabilitySubjectUser, "sub-pm", capFeature, d.value, allow),
				}
				c, b, n, r := try(t, d, &capStore{grants: rows}, pm(t))
				wantRefused(t, d, c, b, n, r)
			})
			t.Run("a deny on the other feature does not bite", func(t *testing.T) {
				rows := []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capFeature, other, deny)}
				c, b, n, r := try(t, d, &capStore{grants: rows}, pm(t))
				wantMinted(t, d, c, b, n, r)
			})
			t.Run("a security admin is bounded like anyone", func(t *testing.T) {
				sec := ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin)
				c, b, n, r := try(t, d, &capStore{enf: enforced()}, sec)
				wantRefused(t, d, c, b, n, r)
			})
			t.Run("a super admin is exempt", func(t *testing.T) {
				admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
				rows := []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capFeature, capWildcard, deny)}
				c, b, n, r := try(t, d, &capStore{enf: enforced(), grants: rows}, admin)
				wantMinted(t, d, c, b, n, r)
			})
			t.Run("a stamped type that no longer exists: refused, nothing written", func(t *testing.T) {
				ghost := ssoSessionOfType(t, "sub-ghost", "ghost@corp.example", oidc.RoleUser, "deleted-type")
				c, b, n, _ := try(t, d, &capStore{}, ghost)
				if c != http.StatusForbidden || n != 0 {
					t.Fatalf("code = %d, rows = %d, want 403 and none: %s", c, n, b)
				}
			})
			t.Run("the grant store failing: 500, nothing written (never allowed)", func(t *testing.T) {
				c, b, n, r := try(t, d, &capStore{err: errors.New("grant store down")}, pm(t))
				if c != http.StatusInternalServerError || n != 0 || len(r) != 0 {
					t.Fatalf("code = %d, rows = %d, denials = %v, want 500, none, none: %s", c, n, r, b)
				}
			})
		})
	}
}

// TestFeatureGrantValueIsTheClosedSet: the mint doors ask about exactly two
// strings, so the write boundary folds case and refuses anything else rather
// than storing a deny that turns nothing off.
func TestFeatureGrantValueIsTheClosedSet(t *testing.T) {
	for in, want := range map[string]string{"ssh_key": "ssh_key", "API_TOKEN": "api_token", " Ssh_Key ": "ssh_key", "*": "*"} {
		if got, err := canonicalGrantValue(capFeature, in); err != nil || got != want {
			t.Errorf("canonicalGrantValue(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"ssh-key", "ssh", "replay", "sſh_key"} {
		if _, err := canonicalGrantValue(capFeature, bad); err == nil {
			t.Errorf("canonicalGrantValue(%q) accepted a value no mint door asks about", bad)
		}
	}
}

// TestSSHKeyFeatureRefusalMatchesConsole: the pane shows the server's
// sentence before the click, so the two must be the same bytes.
func TestSSHKeyFeatureRefusalMatchesConsole(t *testing.T) {
	b, err := os.ReadFile("../../ui/src/app/lib/permissions-copy.ts")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `SSH_KEY_FEATURE: "`+sshKeyFeatureRefusal+`"`) {
		t.Errorf("permissions-copy.ts DENIED.SSH_KEY_FEATURE is not %q", sshKeyFeatureRefusal)
	}
}
