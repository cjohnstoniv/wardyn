// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"os"
	"strings"
	"testing"
	"time"
)

// ─── validateConfig: DSN required + TLS both-or-neither + Secure-cookie posture ──

// TestValidateConfig is the P0 config-validation contract: Postgres DSN is
// mandatory, the TLS cert/key pair is both-or-neither (a half-set pair fails
// closed rather than silently falling back to plain HTTP), and the Secure-cookie
// posture is derived from whether the connection is TLS-protected end to end
// (built-in TLS OR an upstream TLS-terminating proxy).
func TestValidateConfig(t *testing.T) {
	tests := []struct {
		name          string
		dsn           string
		tlsCert       string
		tlsKey        string
		tlsTerminated bool
		wantErr       bool
		// errContains is a substring the error must mention (skipped when empty).
		errContains string
		wantTLS     bool // expected posture.tlsEnabled (only checked on success)
		wantSecure  bool // expected posture.secureCookies (only checked on success)
	}{
		{
			name:    "missing dsn fails closed",
			dsn:     "",
			wantErr: true,
			// the message names both the flag and the env var so an operator can
			// fix it from either surface.
			errContains: "WARDYN_PG_DSN",
		},
		{
			name:       "dsn only, no tls: plain HTTP, cookies not Secure",
			dsn:        "postgres://localhost/wardyn",
			wantTLS:    false,
			wantSecure: false,
		},
		{
			name:       "both cert and key: built-in TLS enabled, cookies Secure",
			dsn:        "postgres://localhost/wardyn",
			tlsCert:    "/etc/wardyn/tls.crt",
			tlsKey:     "/etc/wardyn/tls.key",
			wantTLS:    true,
			wantSecure: true,
		},
		{
			// Half-configured TLS is the security-relevant case: it MUST fail
			// closed, never silently degrade to plain HTTP.
			name:        "cert without key fails closed (both-or-neither)",
			dsn:         "postgres://localhost/wardyn",
			tlsCert:     "/etc/wardyn/tls.crt",
			wantErr:     true,
			errContains: "TLS misconfigured",
		},
		{
			name:        "key without cert fails closed (both-or-neither)",
			dsn:         "postgres://localhost/wardyn",
			tlsKey:      "/etc/wardyn/tls.key",
			wantErr:     true,
			errContains: "TLS misconfigured",
		},
		{
			// WARDYN_TLS_TERMINATED: TLS terminates at an upstream proxy. wardynd
			// serves plain HTTP (tlsEnabled=false) but cookies are still Secure
			// because the browser-facing connection is HTTPS.
			name:          "tls-terminated: plain HTTP locally but cookies Secure",
			dsn:           "postgres://localhost/wardyn",
			tlsTerminated: true,
			wantTLS:       false,
			wantSecure:    true,
		},
		{
			// tlsTerminated alongside built-in TLS is harmless and still Secure;
			// tlsEnabled wins for the listener decision.
			name:          "built-in TLS and tls-terminated both set",
			dsn:           "postgres://localhost/wardyn",
			tlsCert:       "/etc/wardyn/tls.crt",
			tlsKey:        "/etc/wardyn/tls.key",
			tlsTerminated: true,
			wantTLS:       true,
			wantSecure:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			posture, err := validateConfig(tc.dsn, tc.tlsCert, tc.tlsKey, tc.tlsTerminated)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateConfig(%q, %q, %q, %v): want error, got nil",
						tc.dsn, tc.tlsCert, tc.tlsKey, tc.tlsTerminated)
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateConfig(%q, %q, %q, %v): unexpected error: %v",
					tc.dsn, tc.tlsCert, tc.tlsKey, tc.tlsTerminated, err)
			}
			if posture.tlsEnabled != tc.wantTLS {
				t.Errorf("tlsEnabled = %v, want %v", posture.tlsEnabled, tc.wantTLS)
			}
			if posture.secureCookies != tc.wantSecure {
				t.Errorf("secureCookies = %v, want %v", posture.secureCookies, tc.wantSecure)
			}
		})
	}
}

// TestValidateConfig_SecureCookiesNeverOnPlainHTTP pins the most security-
// sensitive invariant on its own: with no built-in TLS and no terminating
// proxy, Secure cookies MUST be false (a Secure cookie is never sent over plain
// HTTP and would break login). Asserted directly so a regression that flips the
// default can never hide inside the larger table.
func TestValidateConfig_SecureCookiesNeverOnPlainHTTP(t *testing.T) {
	posture, err := validateConfig("postgres://localhost/wardyn", "", "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if posture.secureCookies {
		t.Fatal("secureCookies must be false on plain HTTP with no terminating proxy")
	}
	if posture.tlsEnabled {
		t.Fatal("tlsEnabled must be false with no cert/key")
	}
}

// ─── flag vs env precedence for the flagEnv/flagBool/flagDuration helpers ────────
//
// These helpers seed a flag's DEFAULT from the documented env var, then register
// it on flag.CommandLine. So precedence is: an explicit command-line value wins
// over the env (which wins over the compiled-in default). We reset
// flag.CommandLine per case so each helper can be (re)registered without the
// "flag redefined" panic the shared global FlagSet would otherwise produce.

// resetFlags installs a fresh CommandLine so a test can register + Parse flags
// in isolation. ContinueOnError keeps a bad parse from os.Exit-ing the test.
func resetFlags(t *testing.T) {
	t.Helper()
	flag.CommandLine = flag.NewFlagSet("wardynd-test", flag.ContinueOnError)
}

// ensureUnset guarantees an env var is absent for the test, restoring any prior
// value (set vs unset) at test end so the "absent" precedence case is exercised
// faithfully without leaking state to other tests.
func ensureUnset(t *testing.T, key string) {
	t.Helper()
	prev, had := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, prev)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func TestFlagEnv_DefaultUsedWhenEnvAndFlagAbsent(t *testing.T) {
	resetFlags(t)
	// LookupEnv distinguishes unset from empty; ensure the var is genuinely
	// ABSENT so the compiled-in default is used. ensureUnset restores any prior
	// value at test end.
	ensureUnset(t, "WARDYN_TEST_STR")
	p := flagEnv("teststr", "WARDYN_TEST_STR", "compiled-default", "usage")
	if err := flag.CommandLine.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *p != "compiled-default" {
		t.Fatalf("flagEnv with no env, no flag = %q, want compiled-default", *p)
	}
}

func TestFlagEnv_EnvOverridesDefault(t *testing.T) {
	resetFlags(t)
	t.Setenv("WARDYN_TEST_STR", "from-env")
	p := flagEnv("teststr", "WARDYN_TEST_STR", "compiled-default", "usage")
	if err := flag.CommandLine.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *p != "from-env" {
		t.Fatalf("flagEnv with env set = %q, want from-env", *p)
	}
}

func TestFlagEnv_FlagOverridesEnv(t *testing.T) {
	resetFlags(t)
	t.Setenv("WARDYN_TEST_STR", "from-env")
	p := flagEnv("teststr", "WARDYN_TEST_STR", "compiled-default", "usage")
	// An explicit command-line value must win over the env-seeded default.
	if err := flag.CommandLine.Parse([]string{"-teststr=from-flag"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *p != "from-flag" {
		t.Fatalf("explicit flag = %q, want from-flag (flag must beat env)", *p)
	}
}

// Empty env value: LookupEnv returns ok=true for an explicitly-empty var, so
// flagEnv honours it (empty string becomes the default). This is the documented
// behaviour — an operator can blank a value via the env.
func TestFlagEnv_EmptyEnvValueHonoured(t *testing.T) {
	resetFlags(t)
	t.Setenv("WARDYN_TEST_STR", "")
	p := flagEnv("teststr", "WARDYN_TEST_STR", "compiled-default", "usage")
	if err := flag.CommandLine.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *p != "" {
		t.Fatalf("flagEnv with explicit empty env = %q, want empty string", *p)
	}
}

func TestFlagBool_EnvTruthyVariants(t *testing.T) {
	// Any of 1/true/yes/on (case-insensitive, trimmed) is true; 0/false/no/off is
	// false; unset/empty keeps the default. This gates security-relevant toggles
	// (WARDYN_TLS_TERMINATED, the dangerous docker-sock build), so both sets must
	// be exact.
	//
	// An UNRECOGNIZED value is deliberately absent from this table: it no longer
	// resolves to a bool at all. It used to silently become `false` — so a typo
	// (WARDYN_ENVBUILD=treu) quietly turned a feature OFF — and cliutil.FlagBool
	// now exits 2 naming the variable and the value instead. That contract is
	// owned and tested where the exit can be stubbed:
	// internal/cliutil.TestFlagBool_InvalidIsLoud. Re-adding a case here would
	// os.Exit the whole test binary.
	tests := []struct {
		val  string
		want bool
	}{
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"  Yes ", true},
		{"on", true},
		{"0", false},
		{"false", false},
		{"no", false},
		{"off", false},
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.val, func(t *testing.T) {
			resetFlags(t)
			t.Setenv("WARDYN_TEST_BOOL", tc.val)
			p := flagBool("testbool", "WARDYN_TEST_BOOL", false, "usage")
			if err := flag.CommandLine.Parse(nil); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if *p != tc.want {
				t.Fatalf("flagBool(%q) = %v, want %v", tc.val, *p, tc.want)
			}
		})
	}
}

// A falsey env value overrides a compiled-in default of true (the env wins, and
// a non-truthy value resolves to false) — so an operator can turn OFF a toggle
// that defaults on via the env.
func TestFlagBool_EnvOverridesTrueDefault(t *testing.T) {
	resetFlags(t)
	t.Setenv("WARDYN_TEST_BOOL", "no")
	p := flagBool("testbool", "WARDYN_TEST_BOOL", true, "usage")
	if err := flag.CommandLine.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *p {
		t.Fatal("falsey env must override a true default")
	}
}

func TestFlagBool_FlagOverridesEnv(t *testing.T) {
	resetFlags(t)
	t.Setenv("WARDYN_TEST_BOOL", "false")
	p := flagBool("testbool", "WARDYN_TEST_BOOL", false, "usage")
	if err := flag.CommandLine.Parse([]string{"-testbool"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !*p {
		t.Fatal("explicit -testbool must beat a falsey env")
	}
}

func TestFlagDuration_EnvOverridesDefault(t *testing.T) {
	resetFlags(t)
	t.Setenv("WARDYN_TEST_DUR", "45s")
	p := flagDuration("testdur", "WARDYN_TEST_DUR", time.Minute, "usage")
	if err := flag.CommandLine.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *p != 45*time.Second {
		t.Fatalf("flagDuration env = %v, want 45s", *p)
	}
}

// An UNSET env duration keeps the compiled default, quietly.
//
// An UNPARSEABLE one no longer does. This test previously asserted the opposite
// ("fail-safe to default"), which sounded prudent and was not: silently
// reinstating the default means an operator who typos an interval gets a
// DIFFERENT, meaningful setting than they asked for, with nothing said. The
// helper now exits 2 naming the variable and the value. That contract lives
// where the exit can be stubbed: internal/cliutil.TestFlagDuration_InvalidIsLoud.
// Asserting it here would os.Exit the test binary.
func TestFlagDuration_UnsetKeepsDefault(t *testing.T) {
	resetFlags(t)
	p := flagDuration("testdur", "WARDYN_TEST_DUR", 90*time.Second, "usage")
	if err := flag.CommandLine.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *p != 90*time.Second {
		t.Fatalf("unset env should keep default 90s, got %v", *p)
	}
}

func TestFlagDuration_FlagOverridesEnv(t *testing.T) {
	resetFlags(t)
	t.Setenv("WARDYN_TEST_DUR", "45s")
	p := flagDuration("testdur", "WARDYN_TEST_DUR", time.Minute, "usage")
	if err := flag.CommandLine.Parse([]string{"-testdur=10s"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *p != 10*time.Second {
		t.Fatalf("explicit flag = %v, want 10s (flag must beat env)", *p)
	}
}

// ─── -local-trust-forwarder bind cross-check ──

// TestListenBindsSpecificRoutable pins the fail-closed gate for
// -local-trust-forwarder: because that flag DISABLES the loopback-peer check,
// wardynd must refuse to boot when it ALSO binds a specific non-loopback
// interface (private, link-local, or public) — each is a LAN-reachable no-auth
// admin surface. Loopback and the unspecified all-interfaces bind (the compose
// 127.0.0.1-publish topology, which earns a loud log instead) must NOT trip it.
// This is deliberately broader than listenIsRoutablePublic, which excludes
// private/RFC1918 — with the peer gate disabled a private bind is precisely the
// LAN no-auth hole this finding closes.
func TestListenBindsSpecificRoutable(t *testing.T) {
	tests := []struct {
		listen string
		want   bool
	}{
		// specific non-loopback → refuse (the -local-trust-forwarder hole)
		{"203.0.113.5:8080", true},   // public v4
		{"10.0.0.5:8080", true},      // private RFC1918 — listenIsRoutablePublic MISSES this
		{"192.168.1.10:8080", true},  // private RFC1918
		{"172.16.0.1:8080", true},    // private RFC1918
		{"169.254.10.1:8080", true},  // link-local
		{"[2001:db8::1]:8080", true}, // global-unicast v6
		// safe binds → no refusal
		{"127.0.0.1:8080", false}, // loopback
		{"[::1]:8080", false},     // loopback v6
		{"localhost:8080", false}, // loopback name
		{":8080", false},          // unspecified — compose case (loud log, not refusal)
		{"0.0.0.0:8080", false},   // unspecified v4
		{"[::]:8080", false},      // unspecified v6
		{"not-an-ip:8080", false}, // unclassifiable hostname — don't refuse
	}
	for _, tt := range tests {
		if got := listenBindsSpecificRoutable(tt.listen); got != tt.want {
			t.Errorf("listenBindsSpecificRoutable(%q) = %v, want %v", tt.listen, got, tt.want)
		}
	}
}

// ─── standard-AWS fallback for the Bedrock selectors ────────────────────────────
//
// WARDYN_BEDROCK_REGION / _AWS_PROFILE stay authoritative; the standard AWS env
// fills in only where they resolve empty, so a machine already configured for
// AWS needs no Wardyn-specific restatement.

// parseBedrock runs the real parseBootFlags with a clean FlagSet and returns the
// resolved Bedrock selectors, so these tests exercise the shipping code path
// (including the post-parse fallback) rather than a re-implementation.
func parseBedrock(t *testing.T, args ...string) (region, profile string) {
	t.Helper()
	resetFlags(t)
	oldArgs := os.Args
	os.Args = append([]string{"wardynd-test"}, args...)
	t.Cleanup(func() { os.Args = oldArgs })
	f := parseBootFlags()
	return *f.bedrockRegion, *f.bedrockAWSProfile
}

func TestBedrockRegion_FallsBackToStandardAWSEnv(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		wardynRegion, awsRegion      string
		awsDefaultRegion, wantRegion string
	}{
		// The compose case that motivated doing this post-parse: compose always
		// passes WARDYN_BEDROCK_REGION="" , and flagEnv treats an explicitly-empty
		// env as an intentional blank. If the fallback were the flagEnv default
		// argument, this row would yield "" and the feature would be dead in the
		// default deployment mode.
		{"compose empty passthrough still inherits", "", "us-east-1", "", "us-east-1"},
		{"unset inherits AWS_REGION", "", "us-west-2", "", "us-west-2"},
		{"AWS_REGION beats AWS_DEFAULT_REGION", "", "us-west-2", "eu-west-1", "us-west-2"},
		{"AWS_DEFAULT_REGION used when AWS_REGION absent", "", "", "eu-west-1", "eu-west-1"},
		{"WARDYN_BEDROCK_REGION wins over both", "ap-south-1", "us-west-2", "eu-west-1", "ap-south-1"},
		{"all absent stays empty (Bedrock disabled)", "", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range map[string]string{
				"WARDYN_BEDROCK_REGION": tc.wardynRegion,
				"AWS_REGION":            tc.awsRegion,
				"AWS_DEFAULT_REGION":    tc.awsDefaultRegion,
			} {
				if v == "" && k != "WARDYN_BEDROCK_REGION" {
					ensureUnset(t, k)
					continue
				}
				t.Setenv(k, v)
			}
			ensureUnset(t, "AWS_PROFILE")
			ensureUnset(t, "WARDYN_BEDROCK_AWS_PROFILE")
			if got, _ := parseBedrock(t); got != tc.wantRegion {
				t.Fatalf("bedrockRegion = %q, want %q", got, tc.wantRegion)
			}
		})
	}
}

func TestBedrockRegion_FlagBeatsStandardAWSEnv(t *testing.T) {
	t.Setenv("AWS_REGION", "us-west-2")
	ensureUnset(t, "AWS_DEFAULT_REGION")
	ensureUnset(t, "WARDYN_BEDROCK_REGION")
	ensureUnset(t, "AWS_PROFILE")
	ensureUnset(t, "WARDYN_BEDROCK_AWS_PROFILE")
	if got, _ := parseBedrock(t, "-bedrock-region=ca-central-1"); got != "ca-central-1" {
		t.Fatalf("bedrockRegion = %q, want ca-central-1 (explicit flag must win)", got)
	}
}

func TestBedrockAWSProfile_FallsBackToStandardAWSProfile(t *testing.T) {
	ensureUnset(t, "AWS_REGION")
	ensureUnset(t, "AWS_DEFAULT_REGION")
	ensureUnset(t, "WARDYN_BEDROCK_REGION")

	t.Setenv("AWS_PROFILE", "corp-sso")
	t.Setenv("WARDYN_BEDROCK_AWS_PROFILE", "") // compose passthrough shape
	if _, got := parseBedrock(t); got != "corp-sso" {
		t.Fatalf("bedrockAWSProfile = %q, want corp-sso", got)
	}

	t.Setenv("WARDYN_BEDROCK_AWS_PROFILE", "wardyn-only")
	if _, got := parseBedrock(t); got != "wardyn-only" {
		t.Fatalf("bedrockAWSProfile = %q, want wardyn-only (Wardyn-specific must win)", got)
	}
}
