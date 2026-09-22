// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"flag"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/coreos/go-oidc/v3/oidc/oidctest"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// ─── validateConfig: DSN required + TLS both-or-neither + Secure-cookie posture ──

// TestValidateConfig is the P0 config-validation contract: Postgres DSN is
// mandatory, the TLS cert/key pair is both-or-neither (a half-set pair fails
// closed rather than silently falling back to plain HTTP), and the Secure-cookie
// posture is derived from whether the connection is TLS-protected end to end
// (built-in TLS OR an upstream TLS-terminating proxy).
func TestValidateConfig(t *testing.T) {
	tests := []struct {
		name                 string
		dsn                  string
		tlsCert              string
		tlsKey               string
		listen               string // "" behaves like an unspecified bind (listenBindsSpecificRoutable("") is false)
		tlsTerminated        bool
		allowPlaintextListen bool
		wantErr              bool
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
		{
			// The B1a gate: plaintext HTTP on a SPECIFIC non-loopback bind is a
			// real cleartext-admin-API exposure, so it fails closed like the
			// demo-admin-token / -local-trust-forwarder gates.
			name:        "plaintext HTTP on a specific-routable bind is refused",
			dsn:         "postgres://localhost/wardyn",
			listen:      "10.0.0.5:8080",
			wantErr:     true,
			errContains: "WARDYN_ALLOW_PLAINTEXT_LISTEN",
		},
		{
			name:                 "specific-routable bind allowed with the explicit override",
			dsn:                  "postgres://localhost/wardyn",
			listen:               "10.0.0.5:8080",
			allowPlaintextListen: true,
			wantTLS:              false,
			wantSecure:           false,
		},
		{
			name:          "specific-routable bind allowed when TLS terminates upstream",
			dsn:           "postgres://localhost/wardyn",
			listen:        "10.0.0.5:8080",
			tlsTerminated: true,
			wantTLS:       false,
			wantSecure:    true,
		},
		{
			name:       "plaintext HTTP on loopback keeps today's warn-only behavior",
			dsn:        "postgres://localhost/wardyn",
			listen:     "127.0.0.1:8080",
			wantTLS:    false,
			wantSecure: false,
		},
		{
			// make setup / compose bind ":8080" (unspecified) — from inside the
			// container that's indistinguishable from a safe 127.0.0.1-only
			// publish, so it must NOT be refused (boot_flags.go documents the
			// same reasoning for -local-trust-forwarder).
			name:       "plaintext HTTP on the unspecified bind keeps today's warn-only behavior",
			dsn:        "postgres://localhost/wardyn",
			listen:     ":8080",
			wantTLS:    false,
			wantSecure: false,
		},
		{
			// Ordering: the both-or-neither TLS check still fires first even on a
			// specific-routable bind that would otherwise trip the plaintext
			// refusal — a half-set cert/key pair is a config error regardless of
			// where wardynd is listening.
			name:        "half-configured TLS still fails closed on a specific-routable bind",
			dsn:         "postgres://localhost/wardyn",
			tlsCert:     "/etc/wardyn/tls.crt",
			listen:      "10.0.0.5:8080",
			wantErr:     true,
			errContains: "TLS misconfigured",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			posture, err := validateConfig(tc.dsn, tc.tlsCert, tc.tlsKey, tc.listen, tc.tlsTerminated, tc.allowPlaintextListen)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateConfig(%q, %q, %q, %q, %v, %v): want error, got nil",
						tc.dsn, tc.tlsCert, tc.tlsKey, tc.listen, tc.tlsTerminated, tc.allowPlaintextListen)
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateConfig(%q, %q, %q, %q, %v, %v): unexpected error: %v",
					tc.dsn, tc.tlsCert, tc.tlsKey, tc.listen, tc.tlsTerminated, tc.allowPlaintextListen, err)
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
	posture, err := validateConfig("postgres://localhost/wardyn", "", "", "", false, false)
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

// ─── validateOperatorPosture: SSO with no operator allowlist fails closed ────

// TestValidateOperatorPosture is the second boot refusal's contract: configuring
// OIDC SSO IS the declaration that more than one human exists, so an empty
// operator allowlist (which makes every signed-in human admin-equivalent) is
// refused unless WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST overrides it. Unconditional:
// no bind-address escape, unlike the plaintext rule above.
func TestValidateOperatorPosture(t *testing.T) {
	tests := []struct {
		name           string
		oidcConfigured bool
		operatorEmails []string
		allowNoList    bool
		hasRoleMap     bool
		wantErr        bool
	}{
		{
			// The finding: SSO on, nobody named, every human an admin.
			name: "oidc with an empty operator list is refused", oidcConfigured: true, wantErr: true,
		},
		{
			name: "oidc with an operator list boots", oidcConfigured: true, operatorEmails: []string{"ops@corp.example"},
		},
		{
			name: "oidc with an empty list boots with the explicit override", oidcConfigured: true, allowNoList: true,
		},
		{
			// Additive-compatibility guarantee: the override reproduces the exact
			// pre-refusal behavior, allowlist or not.
			name: "override with a list set is still fine", oidcConfigured: true, operatorEmails: []string{"ops@corp.example"}, allowNoList: true,
		},
		{
			// W25-S1-1: the in-repo Entra recipe (SKILL.md step 3) sets
			// WARDYN_OIDC_ROLE_MAP and nothing else — deriveRole switches to
			// claim-based admin/member and no longer needs the allowlist, so
			// this must boot, not crash-loop.
			name: "oidc with a role map and no operator list boots", oidcConfigured: true, hasRoleMap: true,
		},
		{
			// No SSO => the admin token / local mode, one shared credential with no
			// human identity to demote. Nothing to decide, so the rule never fires
			// and the default single-operator deployment is untouched.
			name: "no oidc, no list: unaffected", oidcConfigured: false,
		},
		{
			name: "no oidc with a list set: still unaffected", oidcConfigured: false, operatorEmails: []string{"ops@corp.example"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOperatorPosture(tc.oidcConfigured, tc.operatorEmails, tc.allowNoList, tc.hasRoleMap)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateOperatorPosture(%v, %v, %v): want error, got nil", tc.oidcConfigured, tc.operatorEmails, tc.allowNoList)
				}
				// The message must name BOTH the fix and the override, or an
				// operator hitting this at 3am has to read the source.
				for _, want := range []string{"refusing to start", "WARDYN_OIDC_OPERATOR_EMAILS", "WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not mention %q", err.Error(), want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("validateOperatorPosture(%v, %v, %v): unexpected error: %v", tc.oidcConfigured, tc.operatorEmails, tc.allowNoList, err)
			}
		})
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

// flagEnv/flagBool/flagDuration are cliutil aliases (main.go); their contracts
// are tested on the real symbols in internal/cliutil.

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

// TestResolveLocalMode_RefusesAdminTokenAsLocalOperator is S-06: "admin-token"
// is the reserved MECHANISM principal a per_user row's harness-login refuses
// (internal/api.AdminTokenPrincipal) — a -local-operator seat named the same
// string would collide with it, so it is refused at boot rather than silently
// reading not_applicable / refused-as-a-mechanism at request time.
func TestResolveLocalMode_RefusesAdminTokenAsLocalOperator(t *testing.T) {
	tests := []struct {
		operator string
		wantErr  bool
	}{
		{"admin-token", true},
		{"local:alice", false},
		{"", false}, // defaulted later to defaultLocalOperator(), never this literal
	}
	for _, tt := range tests {
		listen := "127.0.0.1:8080"
		adminToken := ""
		localMode := true
		oidcIssuer := ""
		localTrustFwd := false
		empty := ""
		f := &bootFlags{
			listen:        &listen,
			adminToken:    &adminToken,
			localMode:     &localMode,
			localOperator: &tt.operator,
			oidcIssuer:    &oidcIssuer,
			localTrustFwd: &localTrustFwd,
			// Reached only past the refusal (the two non-erroring cases) — the
			// Bedrock auto-detect tail dereferences these unconditionally.
			bedrockRegion: &empty,
			bedrockModel:  &empty,
			bedrockAWSDir: &empty,
		}
		_, err := resolveLocalMode(f)
		if (err != nil) != tt.wantErr {
			t.Errorf("resolveLocalMode(local-operator=%q) error = %v, want error: %v", tt.operator, err, tt.wantErr)
		}
	}
}

// The demo admin token is published in this repo, so it authenticates nobody:
// refuse to boot with it on a specific routable bind. The loopback and
// unspecified (compose) binds must still boot — they get a warning log instead,
// and every documented demo plus CI runs on them.
func TestResolveLocalModeRefusesPublishedDemoToken(t *testing.T) {
	tests := []struct {
		listen  string
		token   string
		wantErr bool
	}{
		{"10.0.0.5:8080", demoAdminToken, true},    // LAN-reachable admin API on a published token
		{"203.0.113.5:8080", demoAdminToken, true}, // public bind
		{":8080", demoAdminToken, false},           // compose: unspecified bind → warn, still boots
		{"127.0.0.1:8080", demoAdminToken, false},  // loopback demo → warn, still boots
		{"10.0.0.5:8080", "a-real-token", false},   // the operator's own token is their business
	}
	for _, tt := range tests {
		f := &bootFlags{
			listen:        &tt.listen,
			adminToken:    &tt.token,
			localMode:     new(bool),
			localOperator: new(string),
			oidcIssuer:    new(string),
		}
		_, err := resolveLocalMode(f)
		if (err != nil) != tt.wantErr {
			t.Errorf("resolveLocalMode(listen=%q, token=%q) error = %v, want error: %v", tt.listen, tt.token, err, tt.wantErr)
		}
	}
}

// TestResolveLocalMode_RefusesExplicitLocalModeWithOIDC is the bug-rbac-1
// regression: humanOrAdminAuth branches on LocalMode FIRST and bypasses OIDC
// entirely without ever consulting it, so an explicit -local-mode alongside a
// configured -oidc-issuer used to boot clean and silently disable the whole
// configured SSO/RBAC deployment — every request became the fixed
// local:operator, full admin. Refused unless allowLocalModeWithOIDC
// (WARDYN_ALLOW_LOCAL_MODE_WITH_OIDC) explicitly overrides it; the AUTO-
// enable heuristic (no explicit flag, no admin token, loopback bind) is
// untouched — it already excludes a configured issuer on its own.
func TestResolveLocalMode_RefusesExplicitLocalModeWithOIDC(t *testing.T) {
	tests := []struct {
		name      string
		localMode bool
		issuer    string
		override  bool
		wantErr   bool
	}{
		{"explicit local-mode + OIDC configured: refused", true, "https://idp.example.com", false, true},
		{"explicit local-mode + OIDC configured + override: allowed", true, "https://idp.example.com", true, false},
		{"explicit local-mode, no OIDC: unaffected", true, "", false, false},
		{"OIDC configured, local-mode NOT explicitly set (auto-enable path): unaffected", false, "https://idp.example.com", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listen := "127.0.0.1:8080"
			adminToken := ""
			localOperator := ""
			localTrustFwd := false
			empty := ""
			f := &bootFlags{
				listen:                 &listen,
				adminToken:             &adminToken,
				localMode:              &tt.localMode,
				localOperator:          &localOperator,
				localTrustFwd:          &localTrustFwd,
				oidcIssuer:             &tt.issuer,
				allowLocalModeWithOIDC: &tt.override,
				// Reached only past the refusal (override / no-OIDC cases) —
				// the Bedrock auto-detect tail dereferences these unconditionally.
				bedrockRegion: &empty,
				bedrockModel:  &empty,
				bedrockAWSDir: &empty,
			}
			_, err := resolveLocalMode(f)
			if (err != nil) != tt.wantErr {
				t.Errorf("resolveLocalMode(local_mode=%v, oidc_issuer=%q, override=%v) error = %v, want error: %v",
					tt.localMode, tt.issuer, tt.override, err, tt.wantErr)
			}
		})
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

// ─── validateUISandboxConfig: the second origin must actually be a second one ──

// TestValidateUISandboxConfig is the UI-sandbox gateway's boot contract. The
// same-address case is the one that matters: the gateway relays the SANDBOX's
// own pages, and the ONLY thing keeping that code away from the console's
// session is that it arrives on a different browser origin. Bound to the
// console's address, the feature's whole security argument would be false — so
// boot refuses, in terms of what breaks.
func TestValidateUISandboxConfig(t *testing.T) {
	tests := []struct {
		name           string
		uiListen       string
		listen         string
		sshListen      string
		originTemplate string
		posture        tlsPosture
		allowPlaintext bool
		wantErr        string // substring; empty = must succeed
	}{
		{name: "off is always fine", listen: ":8080"},
		{name: "distinct ports", uiListen: ":8081", listen: ":8080"},
		{name: "distinct hosts and ports", uiListen: "127.0.0.1:8081", listen: "127.0.0.1:8080", sshListen: ":2222"},
		{
			name: "identical address refused", uiListen: ":8080", listen: ":8080",
			wantErr: "same address as -listen",
		},
		{
			// ":8080" and "0.0.0.0:8080" are one bind; the refusal must see
			// through the spelling, not compare strings.
			name: "unspecified host still collides", uiListen: "0.0.0.0:8080", listen: ":8080",
			wantErr: "same address as -listen",
		},
		{
			name:     "unspecified UI bind collides with a specific console bind",
			uiListen: ":8080", listen: "127.0.0.1:8080",
			wantErr: "same address as -listen",
		},
		{
			// Two binds, one origin: both listeners come up (different address
			// families), and http://localhost:8080 then serves the console or
			// the sandbox depending on whether the resolver answers A or AAAA
			// first — the same-origin collapse, arrived at sideways.
			name:     "ipv6 loopback collides with the ipv4 loopback on one port",
			uiListen: "[::1]:8080", listen: "127.0.0.1:8080",
			wantErr: "same address as -listen",
		},
		{
			name:     "the localhost name collides with the address it resolves to",
			uiListen: "localhost:8080", listen: "127.0.0.1:8080",
			wantErr: "same address as -listen",
		},
		{
			// The carve-out: two SPECIFIC non-loopback hosts on one port really
			// are two origins (two NICs, two names), so they still boot.
			name:     "two specific non-loopback hosts on one port stay distinct",
			uiListen: "192.168.1.5:8080", listen: "192.168.1.6:8080",
			posture: tlsPosture{tlsEnabled: true, secureCookies: true},
		},
		{
			name: "ssh gateway address collides", uiListen: ":2222", listen: ":8080", sshListen: ":2222",
			wantErr: "same address as -ssh-listen",
		},
		{
			name:     "origin template without a run placeholder refused",
			uiListen: ":8081", listen: ":8080", originTemplate: "https://ui.example.com",
			wantErr: "{run}",
		},
		{
			name: "origin template with a run placeholder", uiListen: ":8081", listen: ":8080",
			originTemplate: "https://run-{run}.ui.example.com",
		},
		{
			// The console's plaintext gate has to cover THIS listener too: the
			// relay cookie is an 8h bearer credential for a run, Secure=false
			// in this posture, and a specific-routable bind puts it on the wire
			// for every LAN peer. Refusing only -listen left the second
			// listener serving exactly what the first one refuses to.
			name:     "plaintext UI gateway on a specific-routable bind is refused",
			uiListen: "192.168.1.5:8081", listen: "127.0.0.1:8080",
			wantErr: "WARDYN_ALLOW_PLAINTEXT_LISTEN",
		},
		{
			name:     "plaintext UI gateway names its own flag in the refusal",
			uiListen: "192.168.1.5:8081", listen: "127.0.0.1:8080",
			wantErr: "-ui-sandbox-listen",
		},
		{
			name:     "routable UI bind is fine with built-in TLS",
			uiListen: "192.168.1.5:8081", listen: "127.0.0.1:8080",
			posture: tlsPosture{tlsEnabled: true, secureCookies: true},
		},
		{
			name:     "routable UI bind is fine behind an upstream terminator",
			uiListen: "192.168.1.5:8081", listen: "127.0.0.1:8080",
			posture: tlsPosture{secureCookies: true},
		},
		{
			name:     "routable UI bind is fine with the explicit override",
			uiListen: "192.168.1.5:8081", listen: "127.0.0.1:8080",
			allowPlaintext: true,
		},
		{
			// Same warn-only carve-outs as the console: compose binds the
			// unspecified address from inside a container.
			name:     "unspecified UI bind stays warn-only",
			uiListen: ":8081", listen: "127.0.0.1:8080",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateUISandboxConfig(tc.uiListen, tc.listen, tc.sshListen, tc.originTemplate, tc.posture, tc.allowPlaintext)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want refused, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestValidateMemberModePosture pins WARDYN_MEMBER_MODE's two preconditions
// (W-MEMB M1). Member mode asserts a topology — the human at the keyboard is a
// MEMBER, the operator authority is elsewhere — and the whole value of the flag
// is that boot REFUSES when the assertion is false. Local mode makes the
// loopback developer an admin; no OIDC means there is no identity to derive a
// member role from. Either one silently inverts the posture, so both fail closed.
func TestValidateMemberModePosture(t *testing.T) {
	for _, tt := range []struct {
		name                                  string
		memberMode, localMode, oidcConfigured bool
		wantErr                               bool
	}{
		{name: "off: nothing asserted, nothing checked", localMode: true},
		{name: "off with no oidc either", memberMode: false},
		{name: "on with oidc and no local mode", memberMode: true, oidcConfigured: true},
		{name: "on with local mode", memberMode: true, localMode: true, oidcConfigured: true, wantErr: true},
		{name: "on without oidc", memberMode: true, wantErr: true},
		{name: "on with both wrong", memberMode: true, localMode: true, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMemberModePosture(tt.memberMode, tt.localMode, tt.oidcConfigured)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateMemberModePosture(member=%v, local=%v, oidc=%v) error = %v, want error: %v",
					tt.memberMode, tt.localMode, tt.oidcConfigured, err, tt.wantErr)
			}
		})
	}
}

// TestValidateHybridPosture covers validateHybridPosture's preconditions
// (issue #100): nil with no org URL, a token with no URL refused, an org URL
// with member mode off refused, and the plaintext/loopback/override scheme
// rule on the URL itself.
func TestValidateHybridPosture(t *testing.T) {
	for _, tt := range []struct {
		name                             string
		orgURL, enrolToken               string
		memberMode, allowPlaintextListen bool
		wantErr                          bool
	}{
		{name: "nothing set: no hybrid posture"},
		{name: "token with no org url is refused", enrolToken: "tok", wantErr: true},
		{name: "org url with member mode off is refused", orgURL: "https://org.example.com", wantErr: true},
		{name: "org url https with member mode on", orgURL: "https://org.example.com", memberMode: true},
		{name: "org url http non-loopback is refused", orgURL: "http://org.example.com", memberMode: true, wantErr: true},
		{name: "org url http loopback is allowed", orgURL: "http://127.0.0.1:9999", memberMode: true},
		{name: "org url http localhost is allowed", orgURL: "http://localhost:9999", memberMode: true},
		{name: "org url http non-loopback with override", orgURL: "http://org.example.com", memberMode: true, allowPlaintextListen: true},
		{name: "malformed org url is refused", orgURL: "http://[::1", memberMode: true, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := validateHybridPosture(tt.orgURL, tt.enrolToken, tt.memberMode, tt.allowPlaintextListen)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateHybridPosture(url=%q, token=%q, member=%v, allowPlaintext=%v) error = %v, want error: %v",
					tt.orgURL, tt.enrolToken, tt.memberMode, tt.allowPlaintextListen, err, tt.wantErr)
			}
		})
	}
}

// TestValidateSSOOnlyPosture pins WARDYN_SSO_ONLY's precondition (#378): SSO
// must actually be the only way in before boot may let /healthz's sso_only bit
// claim it is. One case per way back into "not actually SSO-only", each
// asserted to name the setting to remove, plus the clean pass.
func TestValidateSSOOnlyPosture(t *testing.T) {
	for _, tt := range []struct {
		name                                       string
		ssoOnly, oidcConfigured                    bool
		adminToken                                 string
		localMode, memberMode, allowNoOperatorList bool
		wantErr                                    bool
		wantNames                                  []string
	}{
		{name: "off: nothing asserted, nothing checked", adminToken: "tok", localMode: true},
		{
			name:    "on with oidc and everything else absent: passes",
			ssoOnly: true, oidcConfigured: true,
		},
		{
			name: "on with no oidc configured", ssoOnly: true,
			wantErr: true, wantNames: []string{"WARDYN_SSO_ONLY", "WARDYN_OIDC_ISSUER"},
		},
		{
			name: "on with an admin token set", ssoOnly: true, oidcConfigured: true, adminToken: "tok",
			wantErr: true, wantNames: []string{"WARDYN_SSO_ONLY", "WARDYN_ADMIN_TOKEN"},
		},
		{
			name: "on with local mode set", ssoOnly: true, oidcConfigured: true, localMode: true,
			wantErr: true, wantNames: []string{"WARDYN_SSO_ONLY", "WARDYN_LOCAL_MODE"},
		},
		{
			name: "on with member mode set", ssoOnly: true, oidcConfigured: true, memberMode: true,
			wantErr: true, wantNames: []string{"WARDYN_SSO_ONLY", "WARDYN_MEMBER_MODE"},
		},
		{
			name: "on with the no-operator-list override set", ssoOnly: true, oidcConfigured: true, allowNoOperatorList: true,
			wantErr: true, wantNames: []string{"WARDYN_SSO_ONLY", "WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSSOOnlyPosture(tt.ssoOnly, tt.oidcConfigured, tt.adminToken, tt.localMode, tt.memberMode, tt.allowNoOperatorList)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateSSOOnlyPosture(...) error = %v, want error: %v", err, tt.wantErr)
			}
			for _, want := range tt.wantNames {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err.Error(), want)
				}
			}
		})
	}
}

// ─── validateSSOOnlyPosture, end to end through the real boot path ──────────

// memSecretStore is a minimal in-memory secretstore.Store good enough for
// buildOptionalFeatures to construct a REAL oidc.Authenticator (session key
// bootstrap) without a live Postgres-backed store. Pre-seeded by the caller,
// so Get always finds the key and loadOrCreateSecret's not-found path (which
// checks for pgx.ErrNoRows specifically) is never exercised.
type memSecretStore struct{ vals map[string][]byte }

func (s *memSecretStore) Name() string { return "mem-test" }
func (s *memSecretStore) Put(_ context.Context, name string, value []byte) error {
	s.vals[name] = value
	return nil
}
func (s *memSecretStore) Get(_ context.Context, name string) ([]byte, error) {
	if v, ok := s.vals[name]; ok {
		return v, nil
	}
	return nil, secretstore.ErrNotFound
}
func (s *memSecretStore) Delete(_ context.Context, name string) error {
	delete(s.vals, name)
	return nil
}
func (s *memSecretStore) List(context.Context) ([]string, error) { return nil, nil }
func (s *memSecretStore) For(string) secretstore.Store           { return s }

// ssoOnlyBootFlags builds the *bootFlags a real buildOptionalFeatures call
// needs to reach validateSSOOnlyPosture: a live OIDC issuer (so of.authn is
// genuinely non-nil, not a hand-set bool) plus an operator allowlist (so
// validateOperatorPosture — checked immediately before — passes and never
// masks the assertion this test makes).
func ssoOnlyBootFlags(issuerURL, adminToken string, ssoOnly bool) *bootFlags {
	recordingSel, recordingDir := "off", ""
	oidcInternalIss, oidcClientID, oidcClientSecret := "", "test-client", ""
	oidcRedirectURL := "http://localhost/auth/callback"
	oidcEmailDomains, oidcRoleMap, oidcDefaultRole := "", "", ""
	oidcOperatorEmails := "ops@example.com"
	allowOIDCNoOperatorList, localMode, memberMode := false, false, false
	dirProvider, dirTenant, dirClientID, dirSecret := "", "", "", ""
	envbuild, scanAIAdvisor := false, false
	sshListen, uiListen := "", ""
	return &bootFlags{
		recordingSel:            &recordingSel,
		recordingDir:            &recordingDir,
		oidcIssuer:              &issuerURL,
		oidcInternalIss:         &oidcInternalIss,
		oidcClientID:            &oidcClientID,
		oidcClientSecret:        &oidcClientSecret,
		oidcRedirectURL:         &oidcRedirectURL,
		oidcEmailDomains:        &oidcEmailDomains,
		oidcOperatorEmails:      &oidcOperatorEmails,
		allowOIDCNoOperatorList: &allowOIDCNoOperatorList,
		oidcRoleMap:             &oidcRoleMap,
		oidcDefaultRole:         &oidcDefaultRole,
		adminToken:              &adminToken,
		localMode:               &localMode,
		memberMode:              &memberMode,
		ssoOnly:                 &ssoOnly,
		// Read only past validateSSOOnlyPosture, on the path this test's
		// "boots clean" case takes all the way to the function's return —
		// every one of them off/empty so that path is a no-op, not a panic
		// on a nil pointer this test never meant to exercise.
		dirProvider:   &dirProvider,
		dirTenant:     &dirTenant,
		dirClientID:   &dirClientID,
		dirSecret:     &dirSecret,
		envbuild:      &envbuild,
		scanAIAdvisor: &scanAIAdvisor,
		sshListen:     &sshListen,
		uiListen:      &uiListen,
	}
}

// TestSSOOnlyPosture_WiredThroughTheRealBootPath is the #378 assertion the
// pure-function table test (TestValidateSSOOnlyPosture above) cannot make on
// its own: that boot actually CALLS validateSSOOnlyPosture with the real,
// resolved "is OIDC configured" fact, on a path that constructs a genuine
// oidc.Authenticator against a live (test) IdP — not a hand-set bool a caller
// could drift out of sync with what actually mounted, the same drift class
// TestAWSSSOProxyInject_BootDefaultIsTheKillSwitchConstant below exists to
// catch for the AWS SSO kill switch.
func TestSSOOnlyPosture_WiredThroughTheRealBootPath(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	oidcSrv := &oidctest.Server{PublicKeys: []oidctest.PublicKey{{
		PublicKey: priv.Public(), KeyID: "test-key", Algorithm: "RS256",
	}}}
	httpSrv := httptest.NewServer(oidcSrv)
	defer httpSrv.Close()
	oidcSrv.SetIssuer(httpSrv.URL)

	// Pre-seeded with a valid session key: loadOrCreateSecret's not-found path
	// checks for pgx.ErrNoRows SPECIFICALLY (a Postgres sentinel), so a fake
	// store's secretstore.ErrNotFound would trip its fail-CLOSED default
	// branch instead — pre-seeding sidesteps that path entirely, which is
	// all this test needs: a real Authenticator, not a real bootstrap.
	newStore := func() *memSecretStore {
		return &memSecretStore{vals: map[string][]byte{secretSessionKey: []byte("01234567890123456789012345678901")}}
	}

	t.Run("sso-only with an admin token set: refused", func(t *testing.T) {
		f := ssoOnlyBootFlags(httpSrv.URL, "some-admin-token", true)
		_, err := buildOptionalFeatures(context.Background(), context.Background(), f, nil, newStore(), false, false)
		if err == nil {
			t.Fatal("buildOptionalFeatures: want a refusal booting WARDYN_SSO_ONLY alongside WARDYN_ADMIN_TOKEN, got nil error")
		}
		for _, want := range []string{"WARDYN_SSO_ONLY", "WARDYN_ADMIN_TOKEN"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err.Error(), want)
			}
		}
	})

	t.Run("sso-only with nothing else set: boots clean, real OIDC configured", func(t *testing.T) {
		f := ssoOnlyBootFlags(httpSrv.URL, "", true)
		of, err := buildOptionalFeatures(context.Background(), context.Background(), f, nil, newStore(), false, false)
		if err != nil {
			t.Fatalf("buildOptionalFeatures: unexpected error: %v", err)
		}
		if of.authn == nil {
			t.Fatal("of.authn is nil — the real OIDC discovery round trip against the test IdP did not wire an Authenticator, so this test would have passed the sso-only checks vacuously (oidcConfigured=false)")
		}
	})

	t.Run("an admin token alone (sso-only unset): unaffected", func(t *testing.T) {
		f := ssoOnlyBootFlags(httpSrv.URL, "some-admin-token", false)
		if _, err := buildOptionalFeatures(context.Background(), context.Background(), f, nil, newStore(), false, false); err != nil {
			t.Fatalf("buildOptionalFeatures: unexpected error with sso-only unset: %v", err)
		}
	})
}

// ─── the O-10 kill switch, end to end through the real boot path ────────────────

// THE ONE CONSTANT (general N-new-1). `awsSSOProxyInjectDefaultOn` in
// internal/api/runs_dispatch_sso_inject.go is the whole rollback for Phase B:
// flipping it to false must turn a daemon booted with NOTHING set — no flag, no
// env, which is every deployment that has not opted in — back to the pre-0.7.6
// behaviour. Two pieces have to line up for that, and each was pinned alone:
// api.ResolveAWSSSOProxyInject's parse (internal/api) and the flag's DEFAULT
// string (boot_flags.go). Nothing ran the pair, so a boot default that had drifted
// away from the constant would have left the switch flipped and the lane still on.
func TestAWSSSOProxyInject_BootDefaultIsTheKillSwitchConstant(t *testing.T) {
	ensureUnset(t, "WARDYN_AWS_SSO_PROXY_INJECT")
	resetFlags(t)
	oldArgs := os.Args
	os.Args = []string{"wardynd-test"}
	t.Cleanup(func() { os.Args = oldArgs })

	f := parseBootFlags()
	got := api.ResolveAWSSSOProxyInject(*f.awsSSOProxyInject)
	// The constant itself is unexported and stays that way; ResolveAWSSSOProxyInject
	// of a value nobody typed IS the constant, by its own documented rule, so this
	// reads the switch through the one window the package already exports.
	if want := api.ResolveAWSSSOProxyInject(""); got != want {
		t.Errorf("a daemon booted with nothing set resolves AWSSSOProxyInject = %v, want %v — "+
			"the flag default %q no longer follows the kill-switch constant", got, want, *f.awsSSOProxyInject)
	}
	// …and the switch is REACHABLE: an explicit value still wins over the default,
	// so the constant is a default and not a hard-coding.
	for _, tc := range []struct {
		raw  string
		want bool
	}{{"off", false}, {"on", true}} {
		if got := api.ResolveAWSSSOProxyInject(tc.raw); got != tc.want {
			t.Errorf("WARDYN_AWS_SSO_PROXY_INJECT=%q resolves to %v, want %v", tc.raw, got, tc.want)
		}
	}
}
