// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sshKeysTestServer builds a Server with a real (in-memory) store wired, so
// the REST handlers exercise the actual store.Store surface — not left nil
// the way plain newHarness(t) leaves it.
func sshKeysTestServer(t *testing.T) (*Server, *sshMemStore) {
	t.Helper()
	h := newHarness(t)
	st := newSSHMemStore()
	cfg := baseTestConfig(h, st)
	return New(cfg), st
}

func TestSSHKeysREST_AddListDelete(t *testing.T) {
	srv, _ := sshKeysTestServer(t)

	// Add.
	body := `{"name":"laptop","public_key":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBl3jvXfmZbBd3q5aLKZTv3rIcvKlfz2eYQpuYSGfCPT alice@laptop"}`
	w := do(t, srv, http.MethodPost, "/api/v1/me/ssh-keys", adminToken, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("add key: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var added types.SSHPublicKey
	if err := json.Unmarshal(w.Body.Bytes(), &added); err != nil {
		t.Fatalf("decode added key: %v", err)
	}
	if added.Fingerprint == "" || added.Principal != adminTokenPrincipal || added.Name != "laptop" {
		t.Errorf("added key = %+v, want a fingerprint, principal=%q, name=laptop", added, adminTokenPrincipal)
	}
	// The comment in the pasted line is NOT echoed into PublicKey (it is
	// re-marshaled from the parsed key) but is available as the default Name
	// when Name is omitted — proven in the next subtest.

	// List: exactly the one key just added.
	w = do(t, srv, http.MethodGet, "/api/v1/me/ssh-keys", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("list keys: code = %d, want 200", w.Code)
	}
	var listed []types.SSHPublicKey
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed) != 1 || listed[0].Fingerprint != added.Fingerprint {
		t.Fatalf("list = %+v, want exactly the added key", listed)
	}

	// Delete. FingerprintSHA256 is raw base64 (ssh.FingerprintSHA256), which
	// routinely contains '/' — percent-encode the path segment exactly like a
	// real client must (mirrors lib/api/secrets.ts's
	// encodeURIComponent(name), the same "arbitrary string in a path
	// segment" shape).
	w = do(t, srv, http.MethodDelete, "/api/v1/me/ssh-keys/"+url.PathEscape(added.Fingerprint), adminToken, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete key: code = %d, want 204; body=%s", w.Code, w.Body.String())
	}

	// List again: empty, not null.
	w = do(t, srv, http.MethodGet, "/api/v1/me/ssh-keys", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("list after delete: code = %d, want 200", w.Code)
	}
	if body := w.Body.String(); body != "[]\n" && body != "[]" {
		t.Errorf("list after delete = %q, want an empty array (not null)", body)
	}
}

func TestSSHKeysREST_NameDefaultsToComment(t *testing.T) {
	srv, _ := sshKeysTestServer(t)
	body := `{"public_key":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBl3jvXfmZbBd3q5aLKZTv3rIcvKlfz2eYQpuYSGfCPT alice@laptop"}`
	w := do(t, srv, http.MethodPost, "/api/v1/me/ssh-keys", adminToken, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("add key: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var added types.SSHPublicKey
	_ = json.Unmarshal(w.Body.Bytes(), &added)
	if added.Name != "alice@laptop" {
		t.Errorf("name = %q, want the authorized_keys comment (alice@laptop) when Name is omitted", added.Name)
	}
}

func TestSSHKeysREST_RejectsPrivateKeyMaterial(t *testing.T) {
	srv, _ := sshKeysTestServer(t)
	body := `{"name":"oops","public_key":"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk\n-----END OPENSSH PRIVATE KEY-----"}`
	w := do(t, srv, http.MethodPost, "/api/v1/me/ssh-keys", adminToken, body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	var env struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if !strings.Contains(env.Error, "PRIVATE") {
		t.Errorf("error = %q, want it to name the private-key problem specifically", env.Error)
	}
}

func TestSSHKeysREST_RejectsUnparseableKey(t *testing.T) {
	srv, _ := sshKeysTestServer(t)
	body := `{"name":"garbage","public_key":"not even close to a key"}`
	w := do(t, srv, http.MethodPost, "/api/v1/me/ssh-keys", adminToken, body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

func TestSSHKeysREST_RejectsMoreThanOneKey(t *testing.T) {
	srv, _ := sshKeysTestServer(t)
	twoKeys := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBl3jvXfmZbBd3q5aLKZTv3rIcvKlfz2eYQpuYSGfCPT a\n" +
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBl3jvXfmZbBd3q5aLKZTv3rIcvKlfz2eYQpuYSGfCPT b"
	body, err := json.Marshal(map[string]string{"public_key": twoKeys})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	w := do(t, srv, http.MethodPost, "/api/v1/me/ssh-keys", adminToken, string(body))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code = %d, want 422 (more than one key pasted); body=%s", w.Code, w.Body.String())
	}
}

func TestSSHKeysREST_DuplicateFingerprintConflicts(t *testing.T) {
	srv, _ := sshKeysTestServer(t)
	body := `{"public_key":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBl3jvXfmZbBd3q5aLKZTv3rIcvKlfz2eYQpuYSGfCPT dup"}`
	if w := do(t, srv, http.MethodPost, "/api/v1/me/ssh-keys", adminToken, body); w.Code != http.StatusCreated {
		t.Fatalf("first add: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	w := do(t, srv, http.MethodPost, "/api/v1/me/ssh-keys", adminToken, body)
	if w.Code != http.StatusConflict {
		t.Fatalf("re-add of the identical key: code = %d, want 409; body=%s", w.Code, w.Body.String())
	}
}

// TestSSHKeysREST_ScopedToOwnPrincipal pins the self-service scope: a key
// registered by SOMEONE ELSE (seeded directly at the store, as if a
// different principal had POSTed it) never appears in this caller's list,
// and this caller's DELETE of it 404s exactly like a nonexistent fingerprint
// — no existence leak across principals.
func TestSSHKeysREST_ScopedToOwnPrincipal(t *testing.T) {
	srv, st := sshKeysTestServer(t)
	st.putKey(types.SSHPublicKey{
		Fingerprint: "SHA256:someone-elses-key",
		Principal:   "mallory@example.com",
		Name:        "not yours",
		PublicKey:   "ssh-ed25519 AAAAmallory mallory@host",
	})

	w := do(t, srv, http.MethodGet, "/api/v1/me/ssh-keys", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("list: code = %d, want 200", w.Code)
	}
	var listed []types.SSHPublicKey
	_ = json.Unmarshal(w.Body.Bytes(), &listed)
	if len(listed) != 0 {
		t.Errorf("list = %+v, want empty (mallory's key must not appear in the admin-token principal's list)", listed)
	}

	w = do(t, srv, http.MethodDelete, "/api/v1/me/ssh-keys/SHA256:someone-elses-key", adminToken, "")
	if w.Code != http.StatusNotFound {
		t.Errorf("delete someone else's key: code = %d, want 404 (not 403 — no existence leak)", w.Code)
	}

	// The row must survive: a wrong-principal delete never removed it.
	if _, err := st.GetSSHKeyByFingerprint(context.Background(), "SHA256:someone-elses-key"); err != nil {
		t.Errorf("mallory's key was removed by a non-owner delete attempt: %v", err)
	}
}
