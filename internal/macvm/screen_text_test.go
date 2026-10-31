// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"os"
	"strings"
	"testing"
)

// Setup by hand's window keeps its text byte for byte: screen_darwin.m (cgo, built only on a Mac) holds
// it and shows it when the Go side passes no text, which only the edit does.
func TestSetupWindowTextUnchanged(t *testing.T) {
	b, err := os.ReadFile("screen_darwin.m")
	if err != nil {
		t.Fatal(err)
	}
	m := string(b)
	for _, want := range []string{
		`static NSString *const wardynScreenSteps =
	@"1. Create the account wardyn-admin. Its password is the one-time code.\n"
	@"2. In System Settings > General > Sharing, turn on Screen Sharing. Under its (i) button, turn on "
	@"\"VNC viewers may control screen with password\" (any password; Wardyn replaces it).\n"
	@"3. In the same pane, turn on Remote Login.";`,
		`NSString *t = title == NULL ? @"Wardyn: set up this macOS VM" : `,
		`NSString *s = steps == NULL ? wardynScreenSteps : `,
		// The strip keeps the height of all its steps (#3869).
		"a.automaticallyAdjustsSize = NO;\n\ta.view = box;",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("screen_darwin.m lost setup's window text:\n%s", want)
		}
	}
	if n := strings.Count(m, "wardynScreenSteps"); n != 2 {
		t.Errorf("wardynScreenSteps is used %d times, want its definition and the NULL default", n)
	}
}
