// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/version"
)

// `wardyn version --json` prints the runner hash the release stamped into this
// CLI; the release job compares it to the runner's SHA256SUMS line, and
// `wardyn runner setup` checks the runner it installs against it.
func TestVersionJSONPrintsPinnedRunnerdHash(t *testing.T) {
	const pin = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, tc := range []struct{ name, pinned string }{{"stamped", pin}, {"unstamped", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			old := pinnedRunnerdSHA256
			pinnedRunnerdSHA256 = tc.pinned
			t.Cleanup(func() { pinnedRunnerdSHA256 = old })

			var out bytes.Buffer
			root := rootCmd()
			root.SetOut(&out)
			root.SetArgs([]string{"version", "--json"})
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			var got struct{ Version, RunnerdSHA256 string }
			if err := json.Unmarshal(out.Bytes(), &struct {
				V *string `json:"version"`
				R *string `json:"runnerd_sha256"`
			}{&got.Version, &got.RunnerdSHA256}); err != nil {
				t.Fatalf("not JSON: %q: %v", out.String(), err)
			}
			if got.Version != version.String() || got.RunnerdSHA256 != tc.pinned {
				t.Errorf("got %+v, want version %q and runnerd_sha256 %q", got, version.String(), tc.pinned)
			}
		})
	}
}

func TestVersionPlainPrintsVersion(t *testing.T) {
	var out bytes.Buffer
	root := rootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), version.String()+"\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
