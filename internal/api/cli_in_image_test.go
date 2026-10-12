// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "testing"

func TestCLIInImage(t *testing.T) {
	for _, key := range []string{"base", "claude-code", "codex-cli", "full", "harnesses"} {
		if f := cliInImage(key); f == (cliImageFact{cliInImageUnknown, cliInImageUnknown, cliInImageUnknown}) {
			t.Errorf("%s is a convention image but reads unknown", key)
		}
	}
	for _, key := range []string{"registry.example/team/app:1", "aws-sso", ""} {
		if f := cliInImage(key); f != (cliImageFact{cliInImageUnknown, cliInImageUnknown, cliInImageUnknown}) {
			t.Errorf("%q = %+v, want unknown", key, f)
		}
	}
}
