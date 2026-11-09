// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Trust the list's CAs writes ssl lines for the ca items only. The guest agent's signing certificate, staged
// or not, adds no line and no verify, and with nothing else to trust the request is refused.
func TestEditTrustCAsNeverTrustsTheSigningCert(t *testing.T) {
	rig, dir, stage := newEditRig(t)
	stageCA(t, stage, AgentSigningItem, "Wardyn guest agent signing", true)
	rig.t.run = func(_ context.Context, argv []string) ([]byte, int, error) {
		rig.ran = append(rig.ran, argv)
		return nil, 0, nil
	}
	line := func(name string) string {
		return "/usr/bin/su - " + MacAdminUser + " -c '/usr/bin/sudo /usr/bin/security add-trusted-cert -d -r trustRoot -p ssl -k " +
			MacSystemKeychain + " " + filepath.Join(stage, name, ItemCert) + "'"
	}
	// Only the signing certificate is staged: no ca item, so 409 and nothing run.
	if w := rig.do("POST", "/edit/trust-cas", ""); w.Code != 409 || len(rig.ran) != 0 {
		t.Errorf("signing certificate alone = %d, ran %q, want 409 and nothing run", w.Code, rig.ran)
	}
	// With a ca item: one ssl line for it, none for the signing certificate, and no security call from the agent.
	stageCA(t, stage, "corp", "Corp Root", false)
	w := rig.do("POST", "/edit/trust-cas", "")
	var res EditActionResult
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if w.Code != 200 || res.Certificates != 1 {
		t.Fatalf("CA with a staged signing certificate = %d, %d certificates", w.Code, res.Certificates)
	}
	b, err := os.ReadFile(filepath.Join(dir, EditTrustCAsScript))
	if err != nil {
		t.Fatal(err)
	}
	var su []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "/usr/bin/su ") {
			su = append(su, l)
		}
	}
	if !slices.Equal(su, []string{line("corp")}) {
		t.Errorf("su lines %q, want only %q", su, line("corp"))
	}
	for _, argv := range rig.ran {
		if slices.Contains(argv, "/usr/bin/security") || slices.Contains(argv, "codeSign") {
			t.Errorf("ran %q, want no security or codeSign call", argv)
		}
	}
}
