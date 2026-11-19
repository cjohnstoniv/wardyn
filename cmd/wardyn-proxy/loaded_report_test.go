// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

// Every config source answers the exact bytes it loaded: the file's, stdin's and the env var's,
// spacing and all, so the reported hash is of what the driver handed over and not a re-encoding.
func TestLoadedReport_EverySourceHashesTheExactBytes(t *testing.T) {
	// Indented and with a trailing newline, so a re-marshalled config would hash differently.
	exact := []byte(strings.ReplaceAll(string(validProxyConfigJSON(t)), ",", ",\n  ") + "\n")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, exact, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		path  string
		env   map[string]string
		stdin []byte
	}{
		{name: "file", path: path},
		{name: "stdin", env: map[string]string{configStdinEnv: "1"}, stdin: exact},
		{name: "env", env: map[string]string{"WARDYN_PROXY_CONFIG_JSON": string(exact)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(configStdinEnv, "")
			t.Setenv("WARDYN_PROXY_CONFIG_JSON", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg, raw, err := loadConfig(tc.path, bytes.NewReader(tc.stdin))
			if err != nil || cfg == nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if !bytes.Equal(raw, exact) {
				t.Fatalf("loaded bytes differ from the bytes handed over:\n%q\n%q", raw, exact)
			}
			if got, want := loadedReport(raw).ConfigSHA256, proxy.ConfigSHA256(exact); got != want {
				t.Fatalf("reported hash %s, want %s", got, want)
			}
		})
	}
	t.Setenv(configStdinEnv, "")
	t.Setenv("WARDYN_PROXY_CONFIG_JSON", "")
	if _, _, err := loadConfig("", nil); !errors.Is(err, errNoConfigSource) {
		t.Fatalf("no source: %v", err)
	}
}

// The report names the deployment switches as set in the environment, and only those, with the
// proxy's own version.
func TestLoadedReport_NamesTheSwitchesAndVersion(t *testing.T) {
	t.Setenv("WARDYN_LLM_SCAN", "off")
	t.Setenv("WARDYN_MODEL_USAGE", "")
	os.Unsetenv("WARDYN_MODEL_USAGE") // t.Setenv restores it after
	r := loadedReport([]byte("{}"))
	if len(r.Switches) != 1 || r.Switches["WARDYN_LLM_SCAN"] != "off" || r.ProxyVersion != version.String() {
		t.Fatalf("report %+v", r)
	}
}
