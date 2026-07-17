// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package cliutil

import (
	"bytes"
	"flag"
	"os"
	"strings"
	"testing"
	"time"
)

func TestEnvOr(t *testing.T) {
	t.Setenv("CLIUTIL_TEST_VAR", "")
	if got := EnvOr("CLIUTIL_TEST_VAR", "def"); got != "def" {
		t.Errorf("EnvOr(empty) = %q, want default", got)
	}
	t.Setenv("CLIUTIL_TEST_VAR", "set")
	if got := EnvOr("CLIUTIL_TEST_VAR", "def"); got != "set" {
		t.Errorf("EnvOr(set) = %q, want the env value", got)
	}
}

// resetFlags gives each case a private FlagSet (the helpers register on the
// global flag.CommandLine) and captures flag's error stream so envFatal's
// message is assertable instead of spraying the test log.
func resetFlags(t *testing.T) *bytes.Buffer {
	t.Helper()
	saved := flag.CommandLine
	flag.CommandLine = flag.NewFlagSet(t.Name(), flag.ContinueOnError)
	var out bytes.Buffer
	flag.CommandLine.SetOutput(&out)
	t.Cleanup(func() { flag.CommandLine = saved })
	return &out
}

// stubExit replaces the fatal path so a test can observe that bad input WOULD
// have exited. exit() returning (unlike os.Exit) is fine: the helpers treat the
// fatal branch as terminal and simply fall through to the default.
func stubExit(t *testing.T) *int {
	t.Helper()
	saved := exit
	code := -1
	exit = func(c int) { code = c }
	t.Cleanup(func() { exit = saved })
	return &code
}

// assertLoud pins the whole contract of a fatal env value: exit(2) was called,
// and the message names both the variable and the offending value so the
// operator can find the typo without reading the source.
func assertLoud(t *testing.T, out *bytes.Buffer, code *int, env, val string) {
	t.Helper()
	if *code != 2 {
		t.Fatalf("invalid %s=%q: exit code = %d, want 2 (must fail closed, not silently default)", env, val, *code)
	}
	if msg := out.String(); !strings.Contains(msg, env) || !strings.Contains(msg, val) {
		t.Fatalf("invalid %s=%q: message %q must name the variable and the value", env, val, msg)
	}
}

// ─── FlagBool ──

func TestFlagBool_UnsetKeepsDefaultQuietly(t *testing.T) {
	for _, def := range []bool{false, true} {
		out := resetFlags(t)
		code := stubExit(t)
		t.Setenv("CLIUTIL_TEST_BOOL", "x")
		os.Unsetenv("CLIUTIL_TEST_BOOL") // t.Setenv above registers the restore
		p := FlagBool("b", "CLIUTIL_TEST_BOOL", def, "usage")
		if err := flag.CommandLine.Parse(nil); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if *p != def {
			t.Fatalf("unset env: got %v, want default %v", *p, def)
		}
		if *code != -1 || out.Len() != 0 {
			t.Fatalf("unset env must be silent, got exit=%d out=%q", *code, out.String())
		}
	}
}

// An explicitly-empty value (compose `VAR=` / `docker run -e VAR` passthrough
// of an unset var) is treated as unset, not as "false".
func TestFlagBool_EmptyIsUnset(t *testing.T) {
	out := resetFlags(t)
	code := stubExit(t)
	t.Setenv("CLIUTIL_TEST_BOOL", "")
	p := FlagBool("b", "CLIUTIL_TEST_BOOL", true, "usage")
	if err := flag.CommandLine.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !*p {
		t.Fatal("empty env must keep the true default, not resolve to false")
	}
	if *code != -1 || out.Len() != 0 {
		t.Fatalf("empty env must be silent, got exit=%d out=%q", *code, out.String())
	}
}

func TestFlagBool_ValidValuesParse(t *testing.T) {
	cases := map[string]bool{
		"1": true, "true": true, "TRUE": true, "  Yes ": true, "on": true,
		"0": false, "false": false, "no": false, "OFF": false,
	}
	for val, want := range cases {
		t.Run(val, func(t *testing.T) {
			out := resetFlags(t)
			code := stubExit(t)
			t.Setenv("CLIUTIL_TEST_BOOL", val)
			// Default is the OPPOSITE of want, so a pass proves the env value was
			// parsed rather than the default surviving.
			p := FlagBool("b", "CLIUTIL_TEST_BOOL", !want, "usage")
			if err := flag.CommandLine.Parse(nil); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if *p != want {
				t.Fatalf("FlagBool(%q) = %v, want %v", val, *p, want)
			}
			if *code != -1 || out.Len() != 0 {
				t.Fatalf("valid value must be silent, got exit=%d out=%q", *code, out.String())
			}
		})
	}
}

// THE BUG: a typo used to map to false through the default branch, silently
// disabling whatever the operator was enabling (e.g. WARDYN_ENVBUILD=treu).
func TestFlagBool_InvalidIsLoud(t *testing.T) {
	for _, val := range []string{"treu", "banana", "2", "yes please", "-1"} {
		t.Run(val, func(t *testing.T) {
			out := resetFlags(t)
			code := stubExit(t)
			t.Setenv("CLIUTIL_TEST_BOOL", val)
			FlagBool("b", "CLIUTIL_TEST_BOOL", true, "usage")
			assertLoud(t, out, code, "CLIUTIL_TEST_BOOL", val)
		})
	}
}

func TestFlagBool_FlagOverridesEnv(t *testing.T) {
	resetFlags(t)
	t.Setenv("CLIUTIL_TEST_BOOL", "false")
	p := FlagBool("b", "CLIUTIL_TEST_BOOL", false, "usage")
	if err := flag.CommandLine.Parse([]string{"-b"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !*p {
		t.Fatal("explicit -b must beat a falsey env")
	}
}

// ─── FlagDuration ──

func TestFlagDuration_UnsetKeepsDefaultQuietly(t *testing.T) {
	out := resetFlags(t)
	code := stubExit(t)
	t.Setenv("CLIUTIL_TEST_DUR", "x")
	os.Unsetenv("CLIUTIL_TEST_DUR") // t.Setenv above registers the restore
	p := FlagDuration("d", "CLIUTIL_TEST_DUR", time.Minute, "usage")
	if err := flag.CommandLine.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *p != time.Minute {
		t.Fatalf("unset env: got %v, want 1m", *p)
	}
	if *code != -1 || out.Len() != 0 {
		t.Fatalf("unset env must be silent, got exit=%d out=%q", *code, out.String())
	}
}

func TestFlagDuration_ValidParses(t *testing.T) {
	resetFlags(t)
	t.Setenv("CLIUTIL_TEST_DUR", " 45s ")
	p := FlagDuration("d", "CLIUTIL_TEST_DUR", time.Minute, "usage")
	if err := flag.CommandLine.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *p != 45*time.Second {
		t.Fatalf("FlagDuration env = %v, want 45s", *p)
	}
}

// THE BUG: an unparseable duration used to keep the compiled default, so
// WARDYN_AUTOSTOP_INTERVAL=30 (no unit) ran the reaper on the wrong interval
// with no signal at all.
func TestFlagDuration_InvalidIsLoud(t *testing.T) {
	for _, val := range []string{"not-a-duration", "30", "5 minutes"} {
		t.Run(val, func(t *testing.T) {
			out := resetFlags(t)
			code := stubExit(t)
			t.Setenv("CLIUTIL_TEST_DUR", val)
			FlagDuration("d", "CLIUTIL_TEST_DUR", 90*time.Second, "usage")
			assertLoud(t, out, code, "CLIUTIL_TEST_DUR", val)
		})
	}
}

func TestFlagDuration_FlagOverridesEnv(t *testing.T) {
	resetFlags(t)
	t.Setenv("CLIUTIL_TEST_DUR", "45s")
	p := FlagDuration("d", "CLIUTIL_TEST_DUR", time.Minute, "usage")
	if err := flag.CommandLine.Parse([]string{"-d=10s"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *p != 10*time.Second {
		t.Fatalf("explicit flag = %v, want 10s (flag must beat env)", *p)
	}
}

// ─── FlagIntEnv ──

func TestFlagIntEnv_UnsetKeepsDefaultQuietly(t *testing.T) {
	out := resetFlags(t)
	code := stubExit(t)
	t.Setenv("CLIUTIL_TEST_INT", "x")
	os.Unsetenv("CLIUTIL_TEST_INT") // t.Setenv above registers the restore
	p := FlagIntEnv("n", "CLIUTIL_TEST_INT", 4096, "usage")
	if err := flag.CommandLine.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *p != 4096 {
		t.Fatalf("unset env: got %d, want 4096", *p)
	}
	if *code != -1 || out.Len() != 0 {
		t.Fatalf("unset env must be silent, got exit=%d out=%q", *code, out.String())
	}
}

func TestFlagIntEnv_ValidParses(t *testing.T) {
	resetFlags(t)
	t.Setenv("CLIUTIL_TEST_INT", " 128 ")
	p := FlagIntEnv("n", "CLIUTIL_TEST_INT", 4096, "usage")
	if err := flag.CommandLine.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *p != 128 {
		t.Fatalf("FlagIntEnv env = %d, want 128", *p)
	}
}

// THE BUG: WARDYN_GROUNDTRUTH_BUFFER=4o96 used to silently keep 4096.
func TestFlagIntEnv_InvalidIsLoud(t *testing.T) {
	for _, val := range []string{"4o96", "many", "1.5", "64k"} {
		t.Run(val, func(t *testing.T) {
			out := resetFlags(t)
			code := stubExit(t)
			t.Setenv("CLIUTIL_TEST_INT", val)
			FlagIntEnv("n", "CLIUTIL_TEST_INT", 4096, "usage")
			assertLoud(t, out, code, "CLIUTIL_TEST_INT", val)
		})
	}
}

// ─── EnvBool / EnvDuration (non-flag twins) ──
//
// These register no flag, so there is no flag.Parse() step — the return value
// is read directly. The loudness contract is identical to FlagBool/FlagDuration.

func TestEnvBool_UnsetAndValid(t *testing.T) {
	// unset keeps the default, both directions, quietly.
	for _, def := range []bool{false, true} {
		out := resetFlags(t)
		code := stubExit(t)
		t.Setenv("CLIUTIL_TEST_EBOOL", "x")
		os.Unsetenv("CLIUTIL_TEST_EBOOL")
		if got := EnvBool("CLIUTIL_TEST_EBOOL", def); got != def {
			t.Fatalf("unset env: got %v, want default %v", got, def)
		}
		// empty (compose `VAR=` / docker `-e VAR`) is unset, not false.
		t.Setenv("CLIUTIL_TEST_EBOOL", "")
		if got := EnvBool("CLIUTIL_TEST_EBOOL", def); got != def {
			t.Fatalf("empty env: got %v, want default %v", got, def)
		}
		if *code != -1 || out.Len() != 0 {
			t.Fatalf("unset/empty must be silent, got exit=%d out=%q", *code, out.String())
		}
	}
	cases := map[string]bool{"1": true, "TRUE": true, " Yes ": true, "on": true,
		"0": false, "false": false, "no": false, "OFF": false}
	for val, want := range cases {
		out := resetFlags(t)
		code := stubExit(t)
		t.Setenv("CLIUTIL_TEST_EBOOL", val)
		if got := EnvBool("CLIUTIL_TEST_EBOOL", !want); got != want {
			t.Fatalf("EnvBool(%q) = %v, want %v", val, got, want)
		}
		if *code != -1 || out.Len() != 0 {
			t.Fatalf("valid value %q must be silent, got exit=%d out=%q", val, *code, out.String())
		}
	}
}

// THE BUG: a typo used to map to the default branch, silently disabling a
// security toggle (WARDYN_SUBSCRIPTION_INJECT=of would have stayed ON).
func TestEnvBool_InvalidIsLoud(t *testing.T) {
	for _, val := range []string{"treu", "banana", "2", "yes please", "-1"} {
		t.Run(val, func(t *testing.T) {
			out := resetFlags(t)
			code := stubExit(t)
			t.Setenv("CLIUTIL_TEST_EBOOL", val)
			EnvBool("CLIUTIL_TEST_EBOOL", true)
			assertLoud(t, out, code, "CLIUTIL_TEST_EBOOL", val)
		})
	}
}

func TestEnvDuration_UnsetAndValid(t *testing.T) {
	out := resetFlags(t)
	code := stubExit(t)
	t.Setenv("CLIUTIL_TEST_EDUR", "x")
	os.Unsetenv("CLIUTIL_TEST_EDUR")
	if got := EnvDuration("CLIUTIL_TEST_EDUR", time.Minute); got != time.Minute {
		t.Fatalf("unset env: got %v, want 1m", got)
	}
	t.Setenv("CLIUTIL_TEST_EDUR", " 45s ")
	if got := EnvDuration("CLIUTIL_TEST_EDUR", time.Minute); got != 45*time.Second {
		t.Fatalf("EnvDuration = %v, want 45s", got)
	}
	if *code != -1 || out.Len() != 0 {
		t.Fatalf("unset/valid must be silent, got exit=%d out=%q", *code, out.String())
	}
}

// THE BUG: an unparseable duration used to keep the compiled default, so
// WARDYN_APPROVAL_TIMEOUT=30 (no unit) ran on the wrong timeout with no signal.
func TestEnvDuration_InvalidIsLoud(t *testing.T) {
	for _, val := range []string{"not-a-duration", "30", "5 minutes"} {
		t.Run(val, func(t *testing.T) {
			out := resetFlags(t)
			code := stubExit(t)
			t.Setenv("CLIUTIL_TEST_EDUR", val)
			EnvDuration("CLIUTIL_TEST_EDUR", 90*time.Second)
			assertLoud(t, out, code, "CLIUTIL_TEST_EDUR", val)
		})
	}
}

func TestSplitCSV(t *testing.T) {
	cases := map[string][]string{
		"":          nil,
		"  ":        nil,
		"a,b,c":     {"a", "b", "c"},
		"a, ,b":     {"a", "b"},
		" a , b ,c": {"a", "b", "c"},
		"a.com,":    {"a.com"},
		",,,":       nil,
	}
	for in, want := range cases {
		got := SplitCSV(in)
		if len(got) != len(want) {
			t.Errorf("SplitCSV(%q) = %#v, want %#v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("SplitCSV(%q) = %#v, want %#v", in, got, want)
				break
			}
		}
	}
}
