// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WARDYN_REGISTRY_AUTH_FILE is read at boot: a file naming a credential helper, or one that cannot be read or
// parsed, refuses to start rather than failing every later image check; unset means anonymous registry reads.
func TestRegistryAuthFileCheckedAtBoot(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ok := write("ok.json", `{"auths":{"registry.example.com":{"auth":"dXNlcjpwYXNz"}}}`)
	for path, refuse := range map[string]bool{
		"": false,
		ok: false,
		write("helpers.json", `{"auths":{},"credHelpers":{"registry.example.com":"ecr-login"}}`): true,
		write("store.json", `{"auths":{},"credsStore":"desktop"}`):                               true,
		write("bad.json", `{"auths":`):                                                           true,
		filepath.Join(dir, "missing.json"):                                                       true,
	} {
		reg, err := newImageRegistry(func(k string) string {
			if k == registryAuthFileEnv {
				return path
			}
			return ""
		})
		if (err != nil) != refuse {
			t.Errorf("%q: err = %v, want refused=%v", filepath.Base(path), err, refuse)
		}
		if err != nil && !strings.Contains(err.Error(), registryAuthFileEnv) {
			t.Errorf("%q: boot error %q does not name %s", filepath.Base(path), err, registryAuthFileEnv)
		}
		if err != nil && strings.Contains(err.Error(), "dXNlcjpwYXNz") {
			t.Errorf("boot error carries the credential: %q", err)
		}
		if err == nil && reg == nil {
			t.Errorf("%q: no registry client", filepath.Base(path))
		}
	}
	if registryAuthFileEnv != "WARDYN_REGISTRY_AUTH_FILE" {
		t.Errorf("env name %q", registryAuthFileEnv)
	}
}
