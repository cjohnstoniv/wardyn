// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// --------------------------------------------------------------------------
// ensureLocalSSHKey: local keypair generation/reuse, no network involved.
// --------------------------------------------------------------------------

func TestEnsureLocalSSHKey_GeneratesThenReuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id_ed25519")

	pub1, generated1, err := ensureLocalSSHKey(path)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !generated1 {
		t.Error("first call: generated = false, want true (no key existed yet)")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat private key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("private key mode = %o, want 0600", perm)
	}
	pubInfo, err := os.Stat(path + ".pub")
	if err != nil {
		t.Fatalf("stat public key: %v", err)
	}
	if perm := pubInfo.Mode().Perm(); perm != 0o644 {
		t.Errorf("public key mode = %o, want 0644", perm)
	}
	privBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	pub2, generated2, err := ensureLocalSSHKey(path)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if generated2 {
		t.Error("second call: generated = true, want false (the key already existed on disk)")
	}
	if fp1, fp2 := ssh.FingerprintSHA256(pub1), ssh.FingerprintSHA256(pub2); fp1 != fp2 {
		t.Errorf("fingerprint changed across calls: %s vs %s", fp1, fp2)
	}
	privBytes2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(privBytes) != string(privBytes2) {
		t.Error("private key file content changed on the second call — it was regenerated, not reused")
	}
}

func TestEnsureLocalSSHKey_CorruptExistingFile_ErrorsWithoutOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id_ed25519")
	const garbage = "this is not a private key\n"
	if err := os.WriteFile(path, []byte(garbage), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := ensureLocalSSHKey(path); err == nil {
		t.Fatal("expected an error for a corrupt existing key file")
	} else if !strings.Contains(err.Error(), "not a readable private key") {
		t.Errorf("error = %q, want it to say the file is unreadable rather than silently replacing it", err.Error())
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if string(got) != garbage {
		t.Errorf("file content changed to %q, want the corrupt original left untouched", got)
	}
}

// --------------------------------------------------------------------------
// `ssh-key ensure` against a fake /api/v1/me/ssh-keys.
// --------------------------------------------------------------------------

// sshKeyTestServer stands in for GET/POST /api/v1/me/ssh-keys: existing is
// what GET returns; every POST is recorded (for the "exactly one POST"
// assertions) and answered by postStatus/postBody — 0/nil defaults to 201
// echoing the posted key back with a server-assigned fingerprint.
type sshKeyTestServer struct {
	*httptest.Server
	mu    sync.Mutex
	posts []map[string]string
}

func newSSHKeyTestServer(t *testing.T, existing []sdk.SSHPublicKey, postStatus int, postBody any) *sshKeyTestServer {
	t.Helper()
	s := &sshKeyTestServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/me/ssh-keys" {
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(existing)
		case http.MethodPost:
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.mu.Lock()
			s.posts = append(s.posts, body)
			s.mu.Unlock()
			status := postStatus
			if status == 0 {
				status = http.StatusCreated
			}
			w.WriteHeader(status)
			resp := postBody
			if resp == nil {
				resp = sdk.SSHPublicKey{
					Fingerprint: "SHA256:server-assigned", Principal: "p",
					Name: body["name"], PublicKey: body["public_key"],
					Role: "member", CreatedAt: time.Now().UTC(),
				}
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			t.Fatalf("unexpected method %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(s.Server.Close)
	return s
}

func (s *sshKeyTestServer) postCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.posts)
}

func (s *sshKeyTestServer) lastPost() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.posts) == 0 {
		return nil
	}
	return s.posts[len(s.posts)-1]
}

// runSSHKeyEnsureCmd runs `ssh-key <args>` against srv and returns whatever it
// printed on stdout (--json goes through emitJSON, which targets os.Stdout
// directly rather than cobra's out sink — captureStdout, from
// commands_test.go, is the same fixture the table-writer commands need for
// the same reason).
func runSSHKeyEnsureCmd(t *testing.T, srv *httptest.Server, args ...string) (stdout string, err error) {
	t.Helper()
	cmd := sshKeyCmd(func() *sdk.Client { return &sdk.Client{BaseURL: srv.URL} })
	cmd.SetArgs(args)
	stdout = captureStdout(t, func() { err = cmd.Execute() })
	return stdout, err
}

func TestSSHKeyEnsure_EmptyList_RegistersAndReportsJSONShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id_ed25519")
	srv := newSSHKeyTestServer(t, []sdk.SSHPublicKey{}, 0, nil)

	out, err := runSSHKeyEnsureCmd(t, srv.Server, "ensure", "--path", path, "--name", "testkey", "--json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := srv.postCount(); n != 1 {
		t.Fatalf("posts = %d, want exactly 1", n)
	}
	post := srv.lastPost()
	if len(post) != 2 || post["name"] != "testkey" || post["public_key"] == "" {
		t.Errorf("POST body = %v, want exactly {name: testkey, public_key: <the generated pub line>}", post)
	}

	var res sshKeyEnsureResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("--json output not valid JSON: %v (%q)", err, out)
	}
	if res.Path != path {
		t.Errorf("path = %q, want %q", res.Path, path)
	}
	if res.PublicKey != post["public_key"] {
		t.Errorf("public_key in JSON output = %q, want it to match the POSTed key %q", res.PublicKey, post["public_key"])
	}
	if res.Fingerprint == "" {
		t.Error("fingerprint is empty")
	}
	if !res.Generated {
		t.Error("generated = false, want true (no key file existed yet)")
	}
	if !res.Registered {
		t.Error("registered = false, want true (the fingerprint was not on the account)")
	}
}

func TestSSHKeyEnsure_FingerprintAlreadyListed_NoPOST(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id_ed25519")
	pub, _, err := ensureLocalSSHKey(path) // pre-create the key so its fingerprint is known up front
	if err != nil {
		t.Fatalf("pre-create key: %v", err)
	}
	fp := ssh.FingerprintSHA256(pub)
	srv := newSSHKeyTestServer(t, []sdk.SSHPublicKey{
		{Fingerprint: fp, Principal: "p", Name: "already-here", PublicKey: "existing", Role: "member", CreatedAt: time.Now().UTC()},
	}, 0, nil)

	out, err := runSSHKeyEnsureCmd(t, srv.Server, "ensure", "--path", path, "--json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := srv.postCount(); n != 0 {
		t.Fatalf("posts = %d, want 0 (the fingerprint was already registered)", n)
	}
	var res sshKeyEnsureResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("--json output not valid JSON: %v (%q)", err, out)
	}
	if res.Generated {
		t.Error("generated = true, want false (the key file already existed)")
	}
	if res.Registered {
		t.Error("registered = true, want false (the fingerprint was already on the account)")
	}
	if res.Fingerprint != fp {
		t.Errorf("fingerprint = %q, want %q", res.Fingerprint, fp)
	}
}

func TestSSHKeyEnsure_ServerErrorTextSurfaces(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		msg    string
	}{
		{"422 unprocessable", http.StatusUnprocessableEntity, "not a valid SSH public key: ssh: no key found"},
		{"409 conflict", http.StatusConflict, "unable to register this key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "id_ed25519")
			srv := newSSHKeyTestServer(t, []sdk.SSHPublicKey{}, tc.status, map[string]string{"error": tc.msg})

			_, err := runSSHKeyEnsureCmd(t, srv.Server, "ensure", "--path", path)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("error = %q, want it to contain the server's message %q", err.Error(), tc.msg)
			}
		})
	}
}
