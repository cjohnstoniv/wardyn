// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "testing"

func TestCLIInImage(t *testing.T) {
	// Each column turns yes in the change that installs its tool: gh is in the
	// four images that install it, and the multi-harness image has no recipe here.
	gh := cliImageFact{cliInImageNo, cliInImageNo, cliInImageYes}
	none := cliImageFact{cliInImageNo, cliInImageNo, cliInImageNo}
	for key, want := range map[string]cliImageFact{"base": gh, "claude-code": gh, "codex-cli": gh, "full": gh, "harnesses": none} {
		if f := cliInImage(key); f != want {
			t.Errorf("%s = %+v, want %+v", key, f, want)
		}
	}
	for _, key := range []string{"registry.example/team/app:1", "aws-sso", ""} {
		if f := cliInImage(key); f != (cliImageFact{cliInImageUnknown, cliInImageUnknown, cliInImageUnknown}) {
			t.Errorf("%q = %+v, want unknown", key, f)
		}
	}
}
