// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "testing"

func TestCLIInImage(t *testing.T) {
	// Each column turns yes in the change that installs its tool: aws and gh are
	// in the five images that install them (harnesses layers on claude-code), and az in the two whose recipes
	// install it (scripts/test-image-recipes.py holds those two to one install).
	gh := cliImageFact{cliInImageNo, cliInImageYes, cliInImageYes}
	all := cliImageFact{cliInImageYes, cliInImageYes, cliInImageYes}
	for key, want := range map[string]cliImageFact{"base": gh, "claude-code": gh, "codex-cli": gh, "full": all, "harnesses": all} {
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
