// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !macvm_live

package macvmlive

import (
	"os"
	"regexp"
	"testing"
)

// TestMessagesMatchProduct reads the sign-in refusal from the API source (no build of it) and the
// suite's patterns from harness_test.go, and pins that the suite matches the refusal there and
// never as a placement refusal. It reads the source tree, so it lives outside the live build, whose
// compiled binary runs from any directory; the hosted go (unit) run, from this directory, runs it.
func TestMessagesMatchProduct(t *testing.T) {
	api, err := os.ReadFile("../../../internal/api/run_desktop_vm_signin.go")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`desktopSignInsOffMsg\s*=\s*"([^"]+)"`).FindSubmatch(api)
	if m == nil {
		t.Fatal("no desktopSignInsOffMsg in the API source")
	}
	h, err := os.ReadFile("harness_test.go")
	if err != nil {
		t.Fatal(err)
	}
	pat := func(key string) *regexp.Regexp {
		p := regexp.MustCompile(`"` + key + `":\s*regexp\.MustCompile\(` + "`([^`]+)`").FindSubmatch(h)
		if p == nil {
			t.Fatalf("no %s pattern in harness_test.go", key)
		}
		return regexp.MustCompile(string(p[1]))
	}
	if !pat("signInsOff").Match(m[1]) || pat("placementAny").Match(m[1]) {
		t.Errorf("the suite's sign-in refusal does not match the product's %q, or placementAny takes it", m[1])
	}
}
