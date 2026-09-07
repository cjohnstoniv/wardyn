// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package cliutil

import (
	"flag"
	"strings"
	"testing"
)

// TestFlagEnv_SecretNeverPrintedInUsage is the F157 regression. FlagEnv seeded
// the flag's DEFAULT from the env var, flag.String captures that as
// Flag.DefValue, and PrintDefaults renders a non-empty string default as
// `(default "…")` — printed for -help AND for EVERY parse error, since
// flag.CommandLine is ExitOnError. One typo'd flag in a compose command, a
// systemd unit or a Helm args list therefore wrote WARDYN_ADMIN_TOKEN,
// WARDYN_AGE_KEY (the secret store's master identity),
// WARDYN_OIDC_CLIENT_SECRET or WARDYN_GROUNDTRUTH_TOKEN verbatim to stderr.
func TestFlagEnv_SecretNeverPrintedInUsage(t *testing.T) {
	const secret = "AGE-SECRET-KEY-1QQQQNOTREALBUTSTILLSECRET"
	out := resetFlags(t)
	t.Setenv("CLIUTIL_TEST_SECRET", secret)

	got := FlagEnv("age-key", "CLIUTIL_TEST_SECRET", "", "age X25519 identity for the secret store")
	if err := flag.CommandLine.Parse([]string{"-nosuchflag"}); err == nil {
		t.Fatal("a bad flag must still fail the parse")
	}
	flag.CommandLine.PrintDefaults()

	if strings.Contains(out.String(), secret) {
		t.Fatalf("the env secret reached flag's usage/diagnostic output:\n%s", out.String())
	}
	if *got != secret {
		t.Errorf("value = %q, want the env value (the env must still configure the flag)", *got)
	}
}

// TestFlagEnv_PrecedenceAndCompiledDefault: the visible default stays the
// COMPILED one, an explicit -flag still beats the env, and an unset env still
// leaves the compiled default in place.
func TestFlagEnv_PrecedenceAndCompiledDefault(t *testing.T) {
	out := resetFlags(t)
	t.Setenv("CLIUTIL_TEST_LISTEN", "0.0.0.0:9999")
	got := FlagEnv("listen", "CLIUTIL_TEST_LISTEN", ":8080", "HTTP listen address")
	flag.CommandLine.PrintDefaults()
	if !strings.Contains(out.String(), `(default ":8080")`) {
		t.Errorf("usage lost the compiled default:\n%s", out.String())
	}
	if *got != "0.0.0.0:9999" {
		t.Errorf("env value = %q, want 0.0.0.0:9999", *got)
	}
	if err := flag.CommandLine.Parse([]string{"-listen", "127.0.0.1:1234"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *got != "127.0.0.1:1234" {
		t.Errorf("explicit flag = %q, want it to beat the env value", *got)
	}

	resetFlags(t)
	t.Setenv("CLIUTIL_TEST_LISTEN", "")
	if unset := FlagEnv("listen", "CLIUTIL_TEST_LISTEN", ":8080", "HTTP listen address"); *unset != ":8080" {
		t.Errorf("unset env = %q, want the compiled default", *unset)
	}
}
