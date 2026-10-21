// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A 0.9 governance document is refused by the strict decode, and the refusal names the key 0.10.0
// reads in its place; nothing reaches the server.
func TestGovernanceSet_RefusesRenamedKeys(t *testing.T) {
	fake := newFakeGovernanceServer()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	for old, now := range renamedKeys {
		path := filepath.Join(t.TempDir(), "governance.json")
		doc := `{"` + old + `":[]}`
		if old == "profile_id" || old == "base_profile_id" || old == "profile" {
			doc = `{"units":[{"name":"x","` + old + `":null}]}`
		}
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		root := rootCmd()
		root.SetArgs([]string{"governance", "set", path, "--url", srv.URL, "--token", "tok"})
		root.SetOut(&strings.Builder{})
		root.SetErr(&strings.Builder{})
		err := root.Execute()
		if err == nil || !strings.Contains(err.Error(), `"`+old+`" was renamed in 0.10.0: write "`+now+`"`) {
			t.Errorf("key %q: err = %v, want a refusal naming %q", old, err, now)
		}
	}
}
