// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBrowserLockdownPolicy_OffIsNil(t *testing.T) {
	out, err := browserLockdownPolicy("off", "")
	if err != nil || out != nil {
		t.Fatalf("off = %q, %v; want nil, nil: off delivers nothing", out, err)
	}
	out, err = browserLockdownPolicy("on", "")
	if err != nil {
		t.Fatalf("on with no file = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil || got["ExtensionInstallBlocklist"] == nil {
		t.Fatalf("on with no file = %s, %v; want the baseline", out, err)
	}
}

func TestBrowserLockdownPolicy_RefusesBadInput(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("good.json", `{"URLBlocklist":["example.com"]}`)
	if out, err := browserLockdownPolicy("on", good); err != nil || !strings.Contains(string(out), "URLBlocklist") {
		t.Fatalf("on with a good file = %s, %v; want its keys kept", out, err)
	}
	for _, tc := range []struct {
		name, lockdown, file, want string
	}{
		{"yes", "yes", "", "invalid WARDYN_BROWSER_LOCKDOWN"},
		{"empty", "", "", "invalid WARDYN_BROWSER_LOCKDOWN"},
		{"off plus a file", "off", good, "WARDYN_BROWSER_LOCKDOWN is off"},
		{"on plus a missing file", "on", filepath.Join(dir, "missing.json"), "WARDYN_BROWSER_POLICY_FILE"},
		{"on plus ExtensionSettings", "on", write("ext.json", `{"ExtensionSettings":{"*":{"installation_mode":"allowed"}}}`), "ExtensionSettings"},
	} {
		if out, err := browserLockdownPolicy(tc.lockdown, tc.file); err == nil || !strings.Contains(err.Error(), tc.want) || out != nil {
			t.Errorf("%s: = %q, %v; want a refusal containing %q", tc.name, out, err, tc.want)
		}
	}
}
