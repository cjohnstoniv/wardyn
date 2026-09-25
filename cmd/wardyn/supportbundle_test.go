// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
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
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// redactSecrets: the test proving redaction (D11)

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

// writeTarGz / gatherComposeConfig

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

// capWriter accepts the first n bytes then fails every Write after —
// simulating ENOSPC surfacing only once the buffered tar/gzip output is
// finally flushed (tar's trailer blocks, gzip's footer): every entry's Write
// "succeeds" and the failure lands at Close, exactly the shape this
// describes ("what is lost is the final block + gzip footer + tar trailer").
type capWriter struct{ n int }

func (c *capWriter) Write(p []byte) (int, error) {
	if c.n <= 0 {
		return 0, errors.New("simulated disk full")
	}
	if len(p) > c.n {
		n := c.n
		c.n = 0
		return n, errors.New("simulated disk full")
	}
	c.n -= len(p)
	return len(p), nil
}

// the OLD writeTarGz closed tw/gz/f via three bare `defer`s whose
// errors were never checked — a flush failure at Close (the ENOSPC shape
// above) was silently swallowed and writeTarGz reported success. Close is
// now explicit, in tw -> gz order, and the FIRST error wins.
func TestWriteTarGzWriter_FlushFailureIsNotSwallowed(t *testing.T) {
	// gzip buffers internally and flushes only at Close for input this small,
	// so the whole encoded output lands in one burst well under this cap —
	// there is no room for it to land at all.
	w := &capWriter{n: 50}
	err := writeTarGzWriter(w, map[string][]byte{"a.txt": []byte("hello world")})
	if err == nil {
		t.Fatal("a flush failure at Close was swallowed — writeTarGzWriter returned nil")
	}
	if !strings.Contains(err.Error(), "simulated disk full") {
		t.Errorf("err = %v, want the underlying flush failure surfaced", err)
	}
}

// finalizePartFile is the shared .part+rename mechanism (also used
// by `run recording`'s download) — on a non-nil err it removes the .part
// file and leaves NOTHING at path; on a rename failure it does the same.
func TestFinalizePartFile_ErrorLeavesNoFileAtPathAndRemovesPart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.tar.gz")
	partPath := path + ".part"
	writeFile(t, partPath, "partial content")

	if err := finalizePartFile(partPath, path, errors.New("flush failed")); err == nil {
		t.Fatal("finalizePartFile swallowed the write error")
	}
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Errorf("a file was left at %s after a write failure", path)
	}
	if _, serr := os.Stat(partPath); !os.IsNotExist(serr) {
		t.Errorf(".part file %s was not cleaned up after a write failure", partPath)
	}
}

// The rename half of the same contract: an occupied destination (here, a
// directory sitting at path) makes the rename itself fail — finalizePartFile
// must clean up the .part file exactly the same way.
func TestFinalizePartFile_RenameFailureLeavesNoFileAtPathAndRemovesPart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.tar.gz")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	partPath := path + ".part"
	writeFile(t, partPath, "complete content")

	if err := finalizePartFile(partPath, path, nil); err == nil {
		t.Fatal("finalizePartFile succeeded renaming onto an occupied path")
	}
	if _, serr := os.Stat(partPath); !os.IsNotExist(serr) {
		t.Errorf(".part file %s was not cleaned up after a rename failure", partPath)
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

// gatherProxyDiagnostics: the corporate-proxy field-report triad

func TestGatherProxyDiagnostics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(types.SiteConfig{
			UpstreamProxyURL:     "http://corp-proxy.internal:8080",
			UpstreamProxyNoProxy: []string{"vpce.amazonaws.com", ".corp.internal"},
		})
	}))
	t.Cleanup(srv.Close)

	c := sdk.New(srv.URL, "tok")
	setupRaw := []byte(`{"ready":true,"trusted_ca_certs":3}`)
	got, note := gatherProxyDiagnostics(context.Background(), c, setupRaw)
	if note != "" {
		t.Fatalf("note = %q, want empty", note)
	}
	var diag proxyDiagnostics
	if err := json.Unmarshal(got, &diag); err != nil {
		t.Fatalf("unmarshal: %v; raw=%s", err, got)
	}
	if diag.UpstreamProxyHost != "corp-proxy.internal:8080" {
		t.Errorf("UpstreamProxyHost = %q, want %q", diag.UpstreamProxyHost, "corp-proxy.internal:8080")
	}
	if !slices.Equal(diag.UpstreamProxyBypass, []string{"vpce.amazonaws.com", ".corp.internal"}) {
		t.Errorf("UpstreamProxyBypass = %v", diag.UpstreamProxyBypass)
	}
	if !diag.TrustedCAPresent {
		t.Error("TrustedCAPresent = false, want true (setup-status reported 3 trusted_ca_certs)")
	}
}

// TestGatherProxyDiagnosticsNeverLeaksUserinfo: even if an upstream proxy URL
// somehow carried an embedded credential (write-time validation refuses this
// today — see SiteConfig.UpstreamProxyURL's doc comment — this is
// defense-in-depth against that check ever loosening), url.URL.Host never
// includes the userinfo, so the bundle can only ever carry the bare host.
func TestGatherProxyDiagnosticsNeverLeaksUserinfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(types.SiteConfig{
			UpstreamProxyURL: "http://alice:hunter2@corp-proxy.internal:8080",
		})
	}))
	t.Cleanup(srv.Close)

	c := sdk.New(srv.URL, "tok")
	got, note := gatherProxyDiagnostics(context.Background(), c, []byte(`{}`))
	if note != "" {
		t.Fatalf("note = %q, want empty", note)
	}
	if strings.Contains(string(got), "alice") || strings.Contains(string(got), "hunter2") {
		t.Errorf("proxy diagnostics leaked userinfo: %s", got)
	}
	if !strings.Contains(string(got), "corp-proxy.internal:8080") {
		t.Errorf("proxy diagnostics dropped the host entirely: %s", got)
	}
}

func TestGatherProxyDiagnosticsUnreachableReturnsNote(t *testing.T) {
	c := sdk.New("http://127.0.0.1:0", "tok")
	got, note := gatherProxyDiagnostics(context.Background(), c, []byte(`{}`))
	if got != nil {
		t.Errorf("got = %s, want nil", got)
	}
	if note == "" {
		t.Error("want a non-empty note explaining why nothing was gathered")
	}
}

// end-to-end: the CLI command wired through a fake control plane

func TestSupportBundleCmdEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "version": "0.6.0"})
		case r.URL.Path == "/api/v1/setup/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"ready": true, "trusted_ca_certs": 2})
		case r.URL.Path == "/api/v1/site-config":
			_ = json.NewEncoder(w).Encode(types.SiteConfig{
				UpstreamProxyURL:     "http://corp-proxy.internal:8080",
				UpstreamProxyNoProxy: []string{"vpce.amazonaws.com"},
			})
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
	composeContent := "services:\n  wardynd:\n    environment:\n" +
		"      WARDYN_ADMIN_TOKEN: \"demo-admin-token\"\n" +
		"      APP_PRIVATE_KEY: |\n        first-sensitive-line\n        second-sensitive-line\n" +
		"      WARDYN_AUDIT_SINKS: >-\n        [{\"bearer_token\": \"first-sensitive-line\n        second-sensitive-line\"}]\n"
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

	for _, want := range []string{"cli-version.txt", "healthz.json", "setup-status.json", "audit-tail.json", "compose-config.redacted.yaml", "proxy-config.json"} {
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
	for _, secret := range []string{"demo-admin-token", "first-sensitive-line", "second-sensitive-line"} {
		if strings.Contains(files["compose-config.redacted.yaml"], secret) {
			t.Errorf("compose-config.redacted.yaml leaked %q: %s", secret, files["compose-config.redacted.yaml"])
		}
	}

	// proxy-config.json: the upstream host, its bypass list, and whether a
	// trusted CA was loaded — #144's field-report triad.
	for _, want := range []string{"corp-proxy.internal:8080", "vpce.amazonaws.com", `"trusted_ca_present": true`} {
		if !strings.Contains(files["proxy-config.json"], want) {
			t.Errorf("proxy-config.json = %s, want it to contain %q", files["proxy-config.json"], want)
		}
	}
}

// redactSecrets: the key names and value shapes the first pass missed
//
// `support-bundle`'s own Long text promises "Never
// includes a secret VALUE", and the bundle is gathered from `docker compose
// config` — the LIVE resolved environment, i.e. the real values, not ${VAR}
// placeholders — and then mailed to a support ticket. Three holes:
//
//  1. the marker vocabulary knew PASSWORD/SECRET/TOKEN/_DSN/_KEY/CREDENTIAL and
//     nothing else, so APIKEY / PASSWD / PASSPHRASE / AUTH(ORIZATION) / COOKIE
//     / BEARER key names shipped verbatim;
//  2. only the line's LEADING key was inspected, so a secret nested inside an
//     innocuously-named variable's JSON value survived — including
//     WARDYN_AUDIT_SINKS' webhook bearer_token, which is in the shipped compose
//     file;
//  3. a `--flag=value` argv entry has no bare key at line start at all, so
//     `- --admin-token=...` shipped the token.

func TestRedactSecrets_KeyNamesAndNestedValues(t *testing.T) {
	cases := []struct{ name, in, secret string }{
		// (1) marker vocabulary
		{"APIKEY, no underscore", `      WARDYN_APIKEY: sk-live-abcdef`, "sk-live-abcdef"},
		{"OPENAI_APIKEY", `      OPENAI_APIKEY: sk-REALVALUE`, "sk-REALVALUE"},
		{"bare APIKEY", `      APIKEY: "sk-live-abcdef"`, "sk-live-abcdef"},
		{"PASSWD", `      PGPASSWD: hunter2`, "hunter2"},
		{"MYSQL_PASSWD", `      MYSQL_PASSWD: hunter2`, "hunter2"},
		{"PASSPHRASE list entry", `        - REGISTRY_PASSPHRASE=hunter2`, "hunter2"},
		{"SSH_PASSPHRASE", `      SSH_PASSPHRASE: "correct horse"`, "correct horse"},
		{"AUTH", `      ANTHROPIC_AUTH: Bearer zzz`, "Bearer zzz"},
		{"PROXY_AUTH", `      HTTP_PROXY_AUTH: Basic am9lOnMzY3JldA==`, "am9lOnMzY3JldA=="},
		{"AUTHORIZATION", `      AUTHORIZATION: "Bearer eyJhbGciOi"`, "eyJhbGciOi"},
		{"REGISTRY_AUTH", `      REGISTRY_AUTH: "docker-auth-blob"`, "docker-auth-blob"},
		{"COOKIE", `      SESSION_COOKIE: "abc123"`, "abc123"},
		// (2) nested inside a value whose own key carries no marker — the
		// shipped compose file's audit sink (deploy/compose/docker-compose.yaml).
		{
			"audit-sink webhook bearer",
			`      WARDYN_AUDIT_SINKS: '{"webhook":{"url":"https://siem.corp.example/ingest","bearer_token":"siem-bearer-SUPERSECRET"}}'`,
			"siem-bearer-SUPERSECRET",
		},
		{
			"nested client_secret",
			`      WARDYN_PROXY_CONFIG_JSON: '{"upstream":{"client_secret":"nested-oidc-secret"}}'`,
			"nested-oidc-secret",
		},
		// (3) flag-style argv entry
		{"--flag=value argv", `      - --admin-token=real-flag-token`, "real-flag-token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := string(redactSecrets([]byte(tc.in)))
			if strings.Contains(out, tc.secret) {
				t.Errorf("secret %q survived redaction: %s", tc.secret, out)
			}
			if !strings.Contains(out, "<redacted>") {
				t.Errorf("no <redacted> marker in %q", out)
			}
		})
	}
}

// The counterweight: the diagnostic content a support engineer actually needs
// must still come through. A redactor that eats the whole file is useless.
func TestRedactSecrets_NonSecretLinesSurvive(t *testing.T) {
	for _, line := range []string{
		`    image: ghcr.io/wardyn/wardynd:0.7.0`,
		`      WARDYN_LISTEN: "0.0.0.0:8080"`,
		`      WARDYN_RUNNER: "docker"`,
		`      WARDYN_OIDC_ISSUER: "https://dex:5556/dex"`,
		`      WARDYN_OIDC_REDIRECT_URL: "https://wardyn.example.com/auth/callback"`,
		`      - "127.0.0.1:8080:8080"`,
		`    restart: unless-stopped`,
	} {
		if out := string(redactSecrets([]byte(line))); out != line {
			t.Errorf("non-secret line was modified:\n got %q\nwant %q", out, line)
		}
	}
}
