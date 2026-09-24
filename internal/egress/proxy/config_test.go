// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func baseConfigJSON(t *testing.T, extra map[string]any) []byte {
	t.Helper()
	m := map[string]any{
		"run_id":               uuid.New().String(),
		"control_plane_url":    "https://wardynd:8443",
		"control_plane_ca_pem": testCPCAPEM,
		"run_token":            "tok",
	}
	for k, v := range extra {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestProxyConfig_InternalHosts_RoundTrip: a declared internal host round-trips
// through LoadConfigBytes byte-for-byte, and an empty config carries none —
// the negative control that byte-identical-when-unset holds.
func TestProxyConfig_InternalHosts_RoundTrip(t *testing.T) {
	cfg, err := LoadConfigBytes(baseConfigJSON(t, map[string]any{
		"internal_hosts": []types.InternalHost{
			{HostSuffix: "corp.internal", CIDRs: []string{"10.40.0.0/16"}},
		},
	}))
	if err != nil {
		t.Fatalf("LoadConfigBytes: %v", err)
	}
	if len(cfg.InternalHosts) != 1 || cfg.InternalHosts[0].HostSuffix != "corp.internal" {
		t.Fatalf("InternalHosts did not round-trip: %+v", cfg.InternalHosts)
	}

	cfg, err = LoadConfigBytes(baseConfigJSON(t, nil))
	if err != nil {
		t.Fatalf("LoadConfigBytes (empty): %v", err)
	}
	if len(cfg.InternalHosts) != 0 {
		t.Fatalf("unset internal_hosts must round-trip empty, got %+v", cfg.InternalHosts)
	}
}

// TestProxyConfig_InternalHosts_RejectsNonLiftableCIDR: fail-closed parse-check
// mirrors validateInternalHosts (internal/api) — a config authored outside
// that write path (e.g. a hand-edited file) cannot smuggle a wider CIDR.
func TestProxyConfig_InternalHosts_RejectsNonLiftableCIDR(t *testing.T) {
	_, err := LoadConfigBytes(baseConfigJSON(t, map[string]any{
		"internal_hosts": []types.InternalHost{
			{HostSuffix: "corp.internal", CIDRs: []string{"169.254.0.0/16"}},
		},
	}))
	if err == nil || !strings.Contains(err.Error(), "must lie inside") {
		t.Fatalf("non-liftable CIDR must be rejected at config load, got err=%v", err)
	}
}

// TestProxyConfig_LLMUpstreams_EmptyRoundTrip: a configured gateway round-trips
// through LoadConfigBytes, and unset carries an empty map — byte-identical to
// today (every brokered LLM route dials the vendor host).
func TestProxyConfig_LLMUpstreams_EmptyRoundTrip(t *testing.T) {
	cfg, err := LoadConfigBytes(baseConfigJSON(t, map[string]any{
		"llm_upstreams": map[string]string{anthropicHost: "https://llm-gateway.corp.internal/v1"},
	}))
	if err != nil {
		t.Fatalf("LoadConfigBytes: %v", err)
	}
	if cfg.LLMUpstreams[anthropicHost] != "https://llm-gateway.corp.internal/v1" {
		t.Fatalf("LLMUpstreams did not round-trip: %+v", cfg.LLMUpstreams)
	}

	cfg, err = LoadConfigBytes(baseConfigJSON(t, nil))
	if err != nil {
		t.Fatalf("LoadConfigBytes (empty): %v", err)
	}
	if len(cfg.LLMUpstreams) != 0 {
		t.Fatalf("unset llm_upstreams must round-trip empty, got %+v", cfg.LLMUpstreams)
	}
}

// TestProxyConfig_LLMUpstreams_RejectsMalformedURL: fail-closed parse-check —
// api.ValidateLLMGateways already checked this at boot; a config authored
// outside that path (hand-edited file) still cannot carry garbage.
func TestProxyConfig_LLMUpstreams_RejectsMalformedURL(t *testing.T) {
	_, err := LoadConfigBytes(baseConfigJSON(t, map[string]any{
		"llm_upstreams": map[string]string{anthropicHost: "://not a url"},
	}))
	if err == nil {
		t.Fatal("a malformed llm_upstreams URL must be rejected at config load")
	}
}

// captureConfigLoadLogs runs LoadConfigBytes with a temporary slog default
// so the test can inspect what boot logged, then restores it — same
// mechanism cmd/wardynd's TestBedrockPlainHTTPIsAudibleAtBoot uses.
func captureConfigLoadLogs(t *testing.T, raw []byte) (logged string, cfg *Config, err error) {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	cfg, err = LoadConfigBytes(raw)
	return buf.String(), cfg, err
}

const testSSOPortalHost = "portal.sso.eu-west-2.amazonaws.com"

// TestProxyConfig_WarnsOnUncoveredAWSSSOInjectionHost: an upstream corp proxy
// configured alongside an AWS SSO injection rule, with no bypass entry
// covering the portal host, must WARN at load (never refuse boot — the
// operator may genuinely want that host proxied) so the failure names the
// condition instead of surfacing as a bare CONNECT timeout.
func TestProxyConfig_WarnsOnUncoveredAWSSSOInjectionHost(t *testing.T) {
	logged, cfg, err := captureConfigLoadLogs(t, baseConfigJSON(t, map[string]any{
		"upstream_proxy_url": "http://corp-proxy.internal:8080",
		"injection": []map[string]any{
			{"host": testSSOPortalHost, "header": "x-amz-sso_bearer_token", "grant_id": uuid.New().String()},
		},
	}))
	if err != nil {
		t.Fatalf("LoadConfigBytes: %v", err)
	}
	if cfg.UpstreamProxyURL == "" {
		t.Fatal("upstream_proxy_url did not round-trip")
	}
	if !strings.Contains(logged, testSSOPortalHost) {
		t.Errorf("boot log does not name the uncovered SSO host:\n%s", logged)
	}
}

// TestProxyConfig_NoWarnWhenBypassCoversSSOHost: the negative control — a
// upstream_proxy_no_proxy entry covering the portal's suffix means the SSO
// host will NOT be chained through the upstream, so no warning is due.
func TestProxyConfig_NoWarnWhenBypassCoversSSOHost(t *testing.T) {
	logged, _, err := captureConfigLoadLogs(t, baseConfigJSON(t, map[string]any{
		"upstream_proxy_url":      "http://corp-proxy.internal:8080",
		"upstream_proxy_no_proxy": []string{"amazonaws.com"},
		"injection": []map[string]any{
			{"host": testSSOPortalHost, "header": "x-amz-sso_bearer_token", "grant_id": uuid.New().String()},
		},
	}))
	if err != nil {
		t.Fatalf("LoadConfigBytes: %v", err)
	}
	if strings.Contains(logged, testSSOPortalHost) {
		t.Errorf("a covered SSO host still warned:\n%s", logged)
	}
}

// TestProxyConfig_NoWarnWithoutUpstreamProxy: no corp upstream configured at
// all — the SSO host is never chained through anything, so an uncovered
// bypass list is moot and must not warn.
func TestProxyConfig_NoWarnWithoutUpstreamProxy(t *testing.T) {
	logged, _, err := captureConfigLoadLogs(t, baseConfigJSON(t, map[string]any{
		"injection": []map[string]any{
			{"host": testSSOPortalHost, "header": "x-amz-sso_bearer_token", "grant_id": uuid.New().String()},
		},
	}))
	if err != nil {
		t.Fatalf("LoadConfigBytes: %v", err)
	}
	if strings.Contains(logged, testSSOPortalHost) {
		t.Errorf("no upstream proxy configured, yet it warned about the SSO host:\n%s", logged)
	}
}

// TestProxyConfig_NoWarnForNonSSOInjectionHost: an ordinary (non-AWS-SSO)
// injection host uncovered by the bypass list is NOT this warning's concern
// — it is the operator's own artifact/API host, not the per-user SSO portal
// isAWSSSOPortalHost exists to flag.
func TestProxyConfig_NoWarnForNonSSOInjectionHost(t *testing.T) {
	logged, _, err := captureConfigLoadLogs(t, baseConfigJSON(t, map[string]any{
		"upstream_proxy_url": "http://corp-proxy.internal:8080",
		"injection": []map[string]any{
			{"host": "api.anthropic.com", "header": "Authorization", "grant_id": uuid.New().String()},
		},
	}))
	if err != nil {
		t.Fatalf("LoadConfigBytes: %v", err)
	}
	if strings.Contains(logged, "not covered") {
		t.Errorf("a non-SSO injection host triggered the SSO-coverage warning:\n%s", logged)
	}
}

// An Azure DevOps grant is enforced only on a terminated connection, so a
// config carrying ado_grant without the MITM CA fails at boot instead of
// degrading to a credential-less tunnel.
func TestApplyDefaultsAndValidate_ADOGrantRequiresTheMITMCA(t *testing.T) {
	certPEM, keyPEM := genTestCA(t)
	base := func(cert, key string) *Config {
		return &Config{
			RunID:           uuid.New(),
			ControlPlaneURL: "http://127.0.0.1:8080",
			RunToken:        "tok",
			MITMCACertPEM:   cert,
			MITMCAKeyPEM:    key,
			ADOGrant:        &ADOGrantConfig{},
		}
	}
	for name, cfg := range map[string]*Config{
		"no CA":   base("", ""),
		"no key":  base(string(certPEM), ""),
		"no cert": base("", string(keyPEM)),
	} {
		err := cfg.applyDefaultsAndValidate()
		if err == nil || !strings.Contains(err.Error(), "ado_grant") {
			t.Errorf("%s: err = %v, want a refusal naming ado_grant", name, err)
		}
	}
	if err := base(string(certPEM), string(keyPEM)).applyDefaultsAndValidate(); err != nil {
		t.Fatalf("ado_grant with the MITM CA: %v", err)
	}
}

// A sidecar holds ONE Azure DevOps grant. The gate is keyed by host and every
// organisation shares dev.azure.com, so a second grant in the older ado_grants
// list could only overwrite the first one's organisation pin: the sidecar
// refuses to boot instead of choosing one. A one-entry list, which is all an
// older control plane ever wrote, still loads.
func TestLoadConfig_OneADOGrantPerSidecar(t *testing.T) {
	certPEM, keyPEM := genTestCA(t)
	grant := func(org string) map[string]any {
		return map[string]any{"organization": org, "capabilities": []string{"read"}, "hosts": []string{"dev.azure.com"}}
	}
	load := func(extra map[string]any) (*Config, error) {
		extra["mitm_ca_cert_pem"], extra["mitm_ca_key_pem"] = string(certPEM), string(keyPEM)
		return LoadConfigBytes(baseConfigJSON(t, extra))
	}

	if _, err := load(map[string]any{"ado_grants": []any{grant("acme"), grant("other")}}); err == nil || !strings.Contains(err.Error(), "ado_grants") {
		t.Errorf("two legacy grants: err = %v, want a refusal naming ado_grants", err)
	}
	if _, err := load(map[string]any{"ado_grants": []any{grant("acme")}, "ado_grant": grant("other")}); err == nil || !strings.Contains(err.Error(), "ado_grant") {
		t.Errorf("legacy list beside ado_grant: err = %v, want a refusal", err)
	}

	for key, v := range map[string]any{"ado_grant": grant("acme"), "ado_grants": []any{grant("acme")}} {
		cfg, err := load(map[string]any{key: v})
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		g, ok := newADOGrantsByHost(cfg.ADOGrant).ADOGrantFor("dev.azure.com")
		if !ok || g.Organization != "acme" {
			t.Errorf("%s: grant for dev.azure.com = %+v, %v; want organisation acme", key, g, ok)
		}
	}
}
