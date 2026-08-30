// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── redactSecrets: the test proving redaction (D11) ──────────────────────────

func TestRedactSecrets(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantAbsent []string // substrings that must NOT survive redaction
		wantSame   bool     // line must pass through byte-for-byte unmodified
	}{
		{
			name:       "compose-style DSN with embedded credentials",
			in:         `      WARDYN_PG_DSN: "postgres://wardyn:wardyn-dev@postgres:5432/wardyn?sslmode=disable"`,
			wantAbsent: []string{"wardyn-dev", "wardyn:wardyn-dev"},
		},
		{
			name:       "admin token with a shell-style default",
			in:         `      WARDYN_ADMIN_TOKEN: "${WARDYN_ADMIN_TOKEN:-demo-admin-token}"`,
			wantAbsent: []string{"demo-admin-token"},
		},
		{
			name:       "oidc client secret",
			in:         `      WARDYN_OIDC_CLIENT_SECRET: "${WARDYN_OIDC_CLIENT_SECRET:-wardyn-oidc-secret}"`,
			wantAbsent: []string{"wardyn-oidc-secret"},
		},
		{
			name:       "postgres password, list-style env entry",
			in:         `        - POSTGRES_PASSWORD=wardyn-dev`,
			wantAbsent: []string{"wardyn-dev"},
		},
		{
			name:       "age key",
			in:         `      WARDYN_AGE_KEY: "AGE-SECRET-KEY-1QWERTY"`,
			wantAbsent: []string{"AGE-SECRET-KEY-1QWERTY"},
		},
		{
			name:     "non-secret line passes through untouched",
			in:       `    image: registry:3@sha256:3725021071ec9383eb3d87ddbdff9ed602439b3f7c958c9c2fb941049ea6531d`,
			wantSame: true,
		},
		{
			name:     "a plain hostname env var is not touched",
			in:       `      WARDYN_LISTEN: "0.0.0.0:8080"`,
			wantSame: true,
		},
		{
			name:       "defense-in-depth: DSN creds on a line with no matching key name",
			in:         `      note: see postgres://alice:hunter2@db:5432/x for details`,
			wantAbsent: []string{"hunter2", "alice:hunter2"},
		},
		{
			name:       "commented-out admin token (raw-file fallback path)",
			in:         `      # WARDYN_ADMIN_TOKEN: "real-secret-value"`,
			wantAbsent: []string{"real-secret-value"},
		},
		{
			name:       "double-hash-commented postgres password",
			in:         `## POSTGRES_PASSWORD=hunter2`,
			wantAbsent: []string{"hunter2"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := string(redactSecrets([]byte(tc.in)))
			if tc.wantSame {
				if out != tc.in {
					t.Errorf("line changed: got %q, want unchanged %q", out, tc.in)
				}
				return
			}
			for _, s := range tc.wantAbsent {
				if strings.Contains(out, s) {
					t.Errorf("redacted output still contains secret substring %q: %q", s, out)
				}
			}
			if !strings.Contains(out, "redacted") {
				t.Errorf("redacted output has no <redacted> marker: %q", out)
			}
		})
	}
}

func TestRedactSecretsMultiLineDocument(t *testing.T) {
	doc := "services:\n" +
		"  wardynd:\n" +
		"    image: ghcr.io/wardyn/wardynd:0.6.0\n" +
		"    environment:\n" +
		"      WARDYN_PG_DSN: \"postgres://wardyn:wardyn-dev@postgres:5432/wardyn\"\n" +
		"      WARDYN_ADMIN_TOKEN: \"demo-admin-token\"\n" +
		"      WARDYN_LISTEN: \"0.0.0.0:8080\"\n"

	out := string(redactSecrets([]byte(doc)))
	for _, secret := range []string{"wardyn-dev", "demo-admin-token"} {
		if strings.Contains(out, secret) {
			t.Errorf("document-level redaction still leaked %q:\n%s", secret, out)
		}
	}
	for _, keep := range []string{"ghcr.io/wardyn/wardynd:0.6.0", "0.0.0.0:8080", "wardynd:"} {
		if !strings.Contains(out, keep) {
			t.Errorf("non-secret content was unexpectedly dropped (%q missing):\n%s", keep, out)
		}
	}
}

// ─── writeTarGz / gatherComposeConfig ──────────────────────────────────────────

func TestWriteTarGzRoundTrip(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "bundle.tar.gz")
	files := map[string][]byte{
		"b.txt": []byte("second"),
		"a.txt": []byte("first"),
	}
	if err := writeTarGz(out, files); err != nil {
		t.Fatalf("writeTarGz: %v", err)
	}

	f, err := os.Open(out)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	tr := tar.NewReader(gz)

	got := map[string]string{}
	var order []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar Next: %v", err)
		}
		order = append(order, hdr.Name)
		content, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		got[hdr.Name] = string(content)
	}
	if got["a.txt"] != "first" || got["b.txt"] != "second" {
		t.Errorf("roundtrip content = %v, want a.txt=first b.txt=second", got)
	}
	if len(order) != 2 || order[0] != "a.txt" || order[1] != "b.txt" {
		t.Errorf("entry order = %v, want sorted [a.txt b.txt]", order)
	}
}

func TestGatherComposeConfigFallsBackToRawFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-compose.yaml")
	content := "services:\n  wardynd:\n    environment:\n      WARDYN_ADMIN_TOKEN: \"shh\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write compose file: %v", err)
	}

	// PATH is emptied so `docker` cannot be found, forcing the raw-file
	// fallback deterministically regardless of the host's docker install.
	t.Setenv("PATH", "")

	got, note := gatherComposeConfig(path)
	if note != "" {
		t.Errorf("note = %q, want empty (fallback should have succeeded)", note)
	}
	if string(got) != content {
		t.Errorf("gathered content = %q, want the raw file content %q", got, content)
	}
}

func TestGatherComposeConfigMissingFileReturnsNote(t *testing.T) {
	t.Setenv("PATH", "")
	got, note := gatherComposeConfig(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if got != nil {
		t.Errorf("got = %q, want nil", got)
	}
	if note == "" {
		t.Error("want a non-empty note explaining why nothing was gathered")
	}
}

// ─── end-to-end: the CLI command wired through a fake control plane ───────────

func TestSupportBundleCmdEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "version": "0.6.0"})
		case r.URL.Path == "/api/v1/setup/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"ready": true})
		case strings.HasPrefix(r.URL.Path, "/api/v1/audit"):
			_ = json.NewEncoder(w).Encode([]types.AuditEvent{
				{ID: uuid.New(), Action: "run.dispatch", Outcome: "success", Actor: "alice@corp.example",
					Data: json.RawMessage(`{"secret_looking_field":"should-not-appear"}`)},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	composePath := filepath.Join(dir, "docker-compose.yaml")
	composeContent := "services:\n  wardynd:\n    environment:\n      WARDYN_ADMIN_TOKEN: \"demo-admin-token\"\n"
	if err := os.WriteFile(composePath, []byte(composeContent), 0o644); err != nil {
		t.Fatalf("write compose file: %v", err)
	}
	t.Setenv("PATH", "") // force the raw-file compose fallback, deterministically

	outPath := filepath.Join(dir, "bundle.tar.gz")
	out, err := runCmdWithTimeout(t, func(root *cobra.Command) {
		root.SetArgs([]string{"support-bundle", "--url", srv.URL, "--token", "tok",
			"--out", outPath, "--compose-file", composePath})
	})
	if err != nil {
		t.Fatalf("support-bundle returned error: %v; output=%s", err, out)
	}
	if !strings.Contains(out, outPath) {
		t.Errorf("stdout = %q, want it to mention the written path %q", out, outPath)
	}

	f, err := os.Open(outPath)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	tr := tar.NewReader(gz)
	files := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar Next: %v", err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		files[hdr.Name] = string(b)
	}

	for _, want := range []string{"cli-version.txt", "healthz.json", "setup-status.json", "audit-tail.json", "compose-config.redacted.yaml"} {
		if _, ok := files[want]; !ok {
			t.Errorf("bundle is missing %q; got entries %v", want, slices.Sorted(maps.Keys(files)))
		}
	}
	if !strings.Contains(files["healthz.json"], "0.6.0") {
		t.Errorf("healthz.json = %q, want it to carry the daemon version", files["healthz.json"])
	}
	if strings.Contains(files["audit-tail.json"], "should-not-appear") {
		t.Errorf("audit-tail.json leaked the event's Data payload (must be content-free): %s", files["audit-tail.json"])
	}
	if !strings.Contains(files["audit-tail.json"], "run.dispatch") {
		t.Errorf("audit-tail.json is missing the event's action: %s", files["audit-tail.json"])
	}
	if strings.Contains(files["compose-config.redacted.yaml"], "demo-admin-token") {
		t.Errorf("compose-config.redacted.yaml leaked the admin token: %s", files["compose-config.redacted.yaml"])
	}
}
