// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package assistrelay

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// credentialName is the subscription credential's name inside the harness config dir.
const credentialName = ".credentials.json"

// keptEnv names every variable the harness keeps from the relay's env: PATH, the proxy variables, the CA trust
// variables, and the two that apply_anthropic_mode leaves for the auth mode. Nothing else passes.
var keptEnv = []string{
	"PATH",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "SSL_CERT_DIR", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE",
	"ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY",
}

// memoryNames are what the harness loads as instructions from its working directory and each directory above
// it. None may exist above the isolated working directory.
var memoryNames = []string{"CLAUDE.md", "CLAUDE.local.md", ".claude", "AGENTS.md"}

// Isolation is the directory the harness runs in. Its home and config dir are empty at start, so the harness
// loads no hooks, skills, memory or CLAUDE.md from any config dir.
type Isolation struct {
	// Dir is the harness's HOME.
	Dir string
	// Work is the harness's working directory, empty.
	Work string
	// Config is the harness's CLAUDE_CONFIG_DIR.
	Config string
	// Prompt holds the system prompt.
	Prompt string
}

// NewIsolation creates the directory under root (the system temp dir when root is empty). credential is the
// subscription credential file the auth mode found, or empty in API-key mode: it is reached through a symlink
// in the config dir and never opened, copied or logged.
func NewIsolation(root, credential string) (*Isolation, error) {
	dir, err := os.MkdirTemp(root, "assist-")
	if err != nil {
		return nil, err
	}
	iso := &Isolation{Dir: dir, Work: filepath.Join(dir, "work"), Config: filepath.Join(dir, ".claude"), Prompt: filepath.Join(dir, "prompt")}
	for _, d := range []string{iso.Work, iso.Config} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return nil, err
		}
	}
	if credential != "" {
		if err := os.Symlink(credential, filepath.Join(iso.Config, credentialName)); err != nil {
			return nil, err
		}
	}
	return iso, nil
}

// FindCredential is the subscription credential file, found the way agent-run's detect_anthropic_mode finds
// it, or empty when there is none. It only stats each path.
func FindCredential(getenv func(string) string) string {
	home := getenv("HOME")
	cfg := getenv("CLAUDE_CONFIG_DIR")
	if cfg == "" {
		cfg = filepath.Join(home, ".claude")
	}
	for _, p := range []string{
		filepath.Join(cfg, ".credentials.json"), filepath.Join(cfg, "credentials.json"),
		filepath.Join(home, ".claude", ".credentials.json"), filepath.Join(home, ".claude", "credentials.json"),
	} {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// WritePrompt stores the system prompt of the session that is starting.
func (i *Isolation) WritePrompt(system string) error {
	return os.WriteFile(i.Prompt, []byte(system), 0o600)
}

// WriteSessionMCP writes the MCP config of one session, once, and returns its path. It names only the wardyn
// server, started for that session alone; the file is never rewritten.
func (i *Isolation) WriteSessionMCP(session, self, base string) (string, error) {
	if _, err := uuid.Parse(session); err != nil {
		return "", fmt.Errorf("session id is not a uuid")
	}
	dir := filepath.Join(i.Dir, "s", session)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	cfg := map[string]any{"mcpServers": map[string]any{"wardyn": map[string]any{
		"type": "stdio", "command": self, "args": []string{"tools", "--session", session, "--base", base},
	}}}
	body, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "mcp.json")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return "", err
	}
	return path, f.Close()
}

// ClearStore removes the old conversation: the harness session store and everything else under the config
// dir but the credential symlink, and the harness's cache. Only a reset calls it.
func (i *Isolation) ClearStore() error {
	entries, err := os.ReadDir(i.Config)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == credentialName {
			continue
		}
		if err := os.RemoveAll(filepath.Join(i.Config, e.Name())); err != nil {
			return err
		}
	}
	return os.RemoveAll(filepath.Join(i.Dir, ".cache"))
}

// Env is the harness's env: the kept variables from parent, then HOME and CLAUDE_CONFIG_DIR inside the
// isolated directory and nonessential traffic off.
func (i *Isolation) Env(parent []string) []string {
	var out []string
	for _, kv := range parent {
		name, _, _ := strings.Cut(kv, "=")
		for _, k := range keptEnv {
			if name == k {
				out = append(out, kv)
				break
			}
		}
	}
	return append(out, "HOME="+i.Dir, "CLAUDE_CONFIG_DIR="+i.Config, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
}

// errUnsafeAncestor is the refusal when a directory above the working directory holds instructions the
// harness would load.
var errUnsafeAncestor = errors.New("a directory above the harness's working directory holds instructions it would load")

// CheckAncestors refuses to let the harness start when its working directory, or any directory above it,
// holds a CLAUDE.md, a CLAUDE.local.md or a .claude directory: the harness loads those from every directory
// up to the root. The config dir itself is the one .claude allowed.
func (i *Isolation) CheckAncestors() error {
	for dir := i.Work; ; dir = filepath.Dir(dir) {
		for _, name := range memoryNames {
			p := filepath.Join(dir, name)
			if p == i.Config {
				continue
			}
			if _, err := os.Lstat(p); err == nil {
				return errUnsafeAncestor
			}
		}
		if dir == filepath.Dir(dir) {
			return nil
		}
	}
}
