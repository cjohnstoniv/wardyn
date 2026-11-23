// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package browserpolicy

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestRender_BaselineAlone(t *testing.T) {
	const want = `{"DeveloperToolsAvailability":2,"ExtensionInstallBlocklist":["*"]}`
	for _, org := range [][]byte{nil, []byte("  ")} {
		got, err := Render(org)
		if err != nil {
			t.Fatalf("Render(%q): %v", org, err)
		}
		if string(got) != want {
			t.Fatalf("Render(%q) = %s, want %s", org, got, want)
		}
	}
}

func TestRender_KeepsOrgKeys(t *testing.T) {
	allow := strings.Repeat("a", 32)
	org := []byte(`{"URLBlocklist": ["https://example.com/<x>&y"], "ExtensionInstallAllowlist": ["` + allow + `"]}`)
	got, err := Render(org)
	if err != nil {
		t.Fatal(err)
	}
	var in, out map[string]any
	if err := json.Unmarshal(org, &in); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"URLBlocklist", "ExtensionInstallAllowlist"} {
		if !reflect.DeepEqual(out[k], in[k]) {
			t.Fatalf("%s = %v, want %v", k, out[k], in[k])
		}
	}
	if !reflect.DeepEqual(out["ExtensionInstallBlocklist"], []any{"*"}) {
		t.Fatalf("the baseline blocklist was lost: %s", got)
	}
}

func TestRender_RefusesEveryOwnedKey(t *testing.T) {
	for _, k := range append(slices.Clone(Owned), "ProxyFutureKey") {
		t.Run(k, func(t *testing.T) {
			_, err := Render([]byte(`{"` + k + `": 1}`))
			if err == nil || !strings.Contains(err.Error(), k) {
				t.Fatalf("Render with %s: err = %v, want a refusal naming it", k, err)
			}
		})
	}
}

func TestRender_OwnedCoversTheBaseline(t *testing.T) {
	for k := range baseline() {
		if !slices.Contains(Owned, k) {
			t.Errorf("baseline key %s is not in Owned, so an organisation file could override it", k)
		}
	}
}

func TestRender_RefusesNonObject(t *testing.T) {
	for _, org := range []string{`[]`, `1`, `null`, `{} {}`, `{"a":1} x`} {
		if _, err := Render([]byte(org)); err == nil {
			t.Errorf("Render(%s) was accepted", org)
		}
	}
}

func TestRender_RefusesBadExtensionID(t *testing.T) {
	for _, org := range []string{
		`{"ExtensionInstallAllowlist": ["` + strings.Repeat("a", 31) + `q"]}`,
		`{"ExtensionInstallAllowlist": ["` + strings.Repeat("a", 31) + `"]}`,
		`{"ExtensionInstallAllowlist": "` + strings.Repeat("a", 32) + `"}`,
	} {
		if _, err := Render([]byte(org)); err == nil {
			t.Errorf("Render(%s) was accepted", org)
		}
	}
	if _, err := Render([]byte(`{"ExtensionInstallForcelist": ["` + strings.Repeat("b", 32) + `;https://x"]}`)); err != nil {
		t.Fatalf("a forcelist entry with an update URL was refused: %v", err)
	}
}

func TestRender_TooLarge(t *testing.T) {
	// A valid object of n bytes, so only the size can refuse it.
	sized := func(n int) []byte {
		head, tail := `{"URLBlocklist":"`, `"}`
		return []byte(head + strings.Repeat("x", n-len(head)-len(tail)) + tail)
	}
	over := sized(MaxOrgBytes + 1)
	if len(over) != MaxOrgBytes+1 || !json.Valid(over) {
		t.Fatalf("test input is %d bytes (valid %v), want a valid %d", len(over), json.Valid(over), MaxOrgBytes+1)
	}
	if _, err := Render(over); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("a file over MaxOrgBytes: err = %v, want the size refusal", err)
	}
	if _, err := Render(sized(MaxOrgBytes)); err != nil {
		t.Fatalf("a file of exactly MaxOrgBytes was refused: %v", err)
	}
}
