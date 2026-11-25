// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package assistrelay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// lockedCredential is a credential file nobody but root may open: the file-open spy. Any open of it by the
// relay fails, so an isolation that is built while it stays unread never opened it.
func lockedCredential(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".credentials.json")
	if err := os.WriteFile(p, []byte(`{"claudeAiOauth":{"accessToken":"not-a-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
	return p
}

func TestIsolationLayout(t *testing.T) {
	iso, err := NewIsolation(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{iso.Work, iso.Config} {
		entries, err := os.ReadDir(d)
		if err != nil || len(entries) != 0 {
			t.Fatalf("%s: %v entries, err %v; want an empty dir", d, len(entries), err)
		}
		if filepath.Dir(d) != iso.Dir {
			t.Fatalf("%s is not inside %s", d, iso.Dir)
		}
	}
	if _, err := os.Lstat(filepath.Join(iso.Config, credentialName)); !os.IsNotExist(err) {
		t.Fatalf("API-key mode made a credential entry: %v", err)
	}
}

// TestIsolationSubscriptionCredentialIsASymlink: the entry points at the found file and the file is never
// opened (it is unreadable to anyone but root).
func TestIsolationSubscriptionCredentialIsASymlink(t *testing.T) {
	cred := lockedCredential(t)
	iso, err := NewIsolation(t.TempDir(), cred)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(iso.Config, credentialName)
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("credential entry mode %v, err %v; want a symlink", fi.Mode(), err)
	}
	if got, _ := os.Readlink(link); got != cred {
		t.Fatalf("symlink points at %q, want %q", got, cred)
	}
	if os.Geteuid() != 0 {
		if _, err := os.ReadFile(cred); err == nil {
			t.Fatal("the spy file is readable, so it cannot show an open")
		}
	}
	if err := iso.ClearStore(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("a cleared store lost the credential symlink: %v", err)
	}
}

func TestFindCredentialOrder(t *testing.T) {
	home, cfg := t.TempDir(), t.TempDir()
	env := map[string]string{"HOME": home, "CLAUDE_CONFIG_DIR": cfg}
	getenv := func(k string) string { return env[k] }
	if got := FindCredential(getenv); got != "" {
		t.Fatalf("no file: got %q", got)
	}
	homeCred := filepath.Join(home, ".claude", "credentials.json")
	if err := os.MkdirAll(filepath.Dir(homeCred), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(homeCred, nil, 0); err != nil {
		t.Fatal(err)
	}
	if got := FindCredential(getenv); got != homeCred {
		t.Fatalf("got %q, want the home file %q", got, homeCred)
	}
	cfgCred := filepath.Join(cfg, ".credentials.json")
	if err := os.WriteFile(cfgCred, nil, 0); err != nil {
		t.Fatal(err)
	}
	if got := FindCredential(getenv); got != cfgCred {
		t.Fatalf("got %q, want the config dir's file %q first", got, cfgCred)
	}
}

func TestIsolationEnvKeepsOnlyNamedVariables(t *testing.T) {
	iso, err := NewIsolation(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	parent := []string{
		"PATH=/usr/bin", "HTTP_PROXY=http://wardyn-proxy:3128", "NODE_EXTRA_CA_CERTS=/ca.pem",
		"ANTHROPIC_BASE_URL=http://wardyn-proxy:3128/wardyn/llm/anthropic", "ANTHROPIC_API_KEY=wardyn-proxy-injected",
		"HOME=/home/agent", "CLAUDE_CONFIG_DIR=/home/agent/.claude", "WARDYN_TOOLGATE_CREDENTIAL=secret",
		"WARDYN_ASSISTANT=1", "TERM=xterm",
	}
	want := []string{
		"PATH=/usr/bin", "HTTP_PROXY=http://wardyn-proxy:3128", "NODE_EXTRA_CA_CERTS=/ca.pem",
		"ANTHROPIC_BASE_URL=http://wardyn-proxy:3128/wardyn/llm/anthropic", "ANTHROPIC_API_KEY=wardyn-proxy-injected",
		"HOME=" + iso.Dir, "CLAUDE_CONFIG_DIR=" + iso.Config, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	}
	if got := iso.Env(parent); !reflect.DeepEqual(got, want) {
		t.Fatalf("env = %q\nwant %q", got, want)
	}
}

func TestSessionMCPIsWrittenOnceAndNamesOnlyWardyn(t *testing.T) {
	iso, err := NewIsolation(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	session := uuid.NewString()
	path, err := iso.WriteSessionMCP(session, "/usr/local/bin/wardyn-assist", "http://wardyn-proxy:3128")
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(iso.Dir, "s", session, "mcp.json") {
		t.Fatalf("path %q", path)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	body, _ := os.ReadFile(path)
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	w, ok := cfg.MCPServers["wardyn"]
	if len(cfg.MCPServers) != 1 || !ok || w.Type != "stdio" || w.Command != "/usr/local/bin/wardyn-assist" ||
		!reflect.DeepEqual(w.Args, []string{"tools", "--session", session, "--base", "http://wardyn-proxy:3128"}) {
		t.Fatalf("config = %s", body)
	}
	if _, err := iso.WriteSessionMCP(session, "/other", "http://other"); err == nil {
		t.Fatal("a session's MCP config was written twice")
	}
	if _, err := iso.WriteSessionMCP("../escape", "/x", "http://x"); err == nil {
		t.Fatal("a session id that is not a uuid was accepted")
	}
}

func TestClearStoreRemovesTheConversation(t *testing.T) {
	iso, err := NewIsolation(t.TempDir(), lockedCredential(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{
		filepath.Join(iso.Config, "projects", "-work", "old.jsonl"),
		filepath.Join(iso.Config, ".claude.json"),
		filepath.Join(iso.Dir, ".cache", "claude-cli-nodejs", "log.jsonl"),
	} {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := iso.ClearStore(); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(iso.Config)
	if len(entries) != 1 || entries[0].Name() != credentialName {
		t.Fatalf("config dir after clear: %v; want only the credential symlink", entries)
	}
	if _, err := os.Stat(filepath.Join(iso.Dir, ".cache")); !os.IsNotExist(err) {
		t.Fatalf("cache survived: %v", err)
	}
}

func TestCheckAncestorsRefusesInstructionsAbove(t *testing.T) {
	for _, name := range []string{"CLAUDE.md", "CLAUDE.local.md", ".claude", "AGENTS.md"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			iso, err := NewIsolation(root, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := iso.CheckAncestors(); err != nil {
				t.Skipf("a directory above the test's temp dir already holds instructions: %v", err)
			}
			p := filepath.Join(root, name)
			if strings.HasPrefix(name, ".") {
				err = os.Mkdir(p, 0o700)
			} else {
				err = os.WriteFile(p, []byte("instructions"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := iso.CheckAncestors(); err == nil {
				t.Fatalf("%s above the working directory was not refused", name)
			}
		})
	}
	iso, err := NewIsolation(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(iso.Work, "CLAUDE.md"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := iso.CheckAncestors(); err == nil {
		t.Fatal("a CLAUDE.md in the working directory itself was not refused")
	}
}
