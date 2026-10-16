// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeContext stores a docker CLI context the way the CLI does (meta/<sha256 of its name>/meta.json).
func writeContext(t *testing.T, home, name, host string) {
	t.Helper()
	sum := sha256.Sum256([]byte(name))
	dir := filepath.Join(home, ".docker", "contexts", "meta", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"Name":"` + name + `","Endpoints":{"docker":{"Host":"` + host + `","SkipTLSVerify":false}}}`
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
}

func selectContext(t *testing.T, home, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, ".docker"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".docker", "config.json"), []byte(`{"currentContext":"`+name+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The runner reaches the engine the docker CLI would: DOCKER_CONTEXT, else DOCKER_HOST, else the selected
// context; only a unix-socket context can be followed, and any other is refused with the remedy.
func TestResolveDockerHost(t *testing.T) {
	home := t.TempDir()
	writeContext(t, home, "rancher-desktop", "unix:///home/me/.rd/docker.sock")
	writeContext(t, home, "build-box", "ssh://me@build.example")
	writeContext(t, home, "tls-engine", "tcp://engine.example:2376")
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	for _, tc := range []struct {
		name     string
		selected string // the context in config.json
		env      func(string) string
		want     string
		refusal  string
	}{
		{name: "nothing selected", env: env()},
		{name: "DOCKER_HOST alone", env: env("DOCKER_HOST", "unix:///var/run/other.sock"), want: "unix:///var/run/other.sock"},
		{name: "the default context", selected: "default", env: env("DOCKER_HOST", "unix:///a.sock"), want: "unix:///a.sock"},
		{name: "a selected unix context", selected: "rancher-desktop", env: env(), want: "unix:///home/me/.rd/docker.sock"},
		{name: "DOCKER_HOST outranks a selected context", selected: "build-box", env: env("DOCKER_HOST", "unix:///a.sock"), want: "unix:///a.sock"},
		{name: "DOCKER_CONTEXT outranks DOCKER_HOST", env: env("DOCKER_CONTEXT", "rancher-desktop", "DOCKER_HOST", "unix:///a.sock"), want: "unix:///home/me/.rd/docker.sock"},
		{name: "DOCKER_CONTEXT naming a remote context", env: env("DOCKER_CONTEXT", "build-box", "DOCKER_HOST", "unix:///a.sock"), refusal: "ssh://me@build.example"},
		{name: "a selected tcp context", selected: "tls-engine", env: env(), refusal: "tcp://engine.example:2376"},
		{name: "a context that does not exist", selected: "gone", env: env(), refusal: "not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selectContext(t, home, tc.selected)
			got, err := resolveDockerHost(tc.env, home)
			switch {
			case tc.refusal != "" && (err == nil || !strings.Contains(err.Error(), tc.refusal) || !strings.Contains(err.Error(), "DOCKER_HOST")):
				t.Fatalf("resolved %q, %v; want a refusal naming %q and the DOCKER_HOST remedy", got, err, tc.refusal)
			case tc.refusal == "" && (err != nil || got != tc.want):
				t.Fatalf("resolved %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestSetupRefusesDockerVariablesTheServiceWouldNotSee(t *testing.T) {
	for _, v := range untransportedDockerEnv {
		err := checkUntransportedDockerEnv(func(k string) string {
			if k == v {
				return "1"
			}
			return ""
		})
		if err == nil || !strings.Contains(err.Error(), v) {
			t.Errorf("%s set: %v; want a refusal naming it", v, err)
		}
	}
	if err := checkUntransportedDockerEnv(func(string) string { return "" }); err != nil {
		t.Errorf("nothing set: %v", err)
	}
}
