// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// azureConfig is a loadable config with one gate, one pinned TLS-only rule on its host and the MITM CA;
// each case below breaks one thing.
func azureConfig(t *testing.T) *Config {
	t.Helper()
	certPEM, keyPEM := genTestCA(t)
	return &Config{
		RunID:           uuid.New(),
		ControlPlaneURL: "http://127.0.0.1:8080",
		RunToken:        "tok",
		MITMCACertPEM:   string(certPEM),
		MITMCAKeyPEM:    string(keyPEM),
		AzureGates: []AzureGateConfig{
			{Host: azHost, Route: types.AzureRouteAnthropic, Models: []string{azMain}},
		},
		Injection: []InjectionConfig{{
			InjectionRule: egress.InjectionRule{Host: azHost, RequireTLS: true, PinRoutes: AzureRoutePins(types.AzureRouteAnthropic)},
			GrantID:       uuid.New(),
		}},
	}
}

// A config the gate could not honour fails the sidecar at boot, loudly, instead of a credential riding an
// ungated path.
func TestApplyDefaultsAndValidate_AzureGates(t *testing.T) {
	if err := azureConfig(t).applyDefaultsAndValidate(); err != nil {
		t.Fatalf("a well-formed Azure config: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"no CA":                func(c *Config) { c.MITMCACertPEM, c.MITMCAKeyPEM = "", "" },
		"no host":              func(c *Config) { c.AzureGates[0].Host = " " },
		"unknown route":        func(c *Config) { c.AzureGates[0].Route = "chat" },
		"no models":            func(c *Config) { c.AzureGates[0].Models = nil },
		"empty model":          func(c *Config) { c.AzureGates[0].Models = []string{azMain, ""} },
		"negative cap":         func(c *Config) { c.AzureGates[0].BodyCap = -1 },
		"cap above the budget": func(c *Config) { c.AzureGates[0].BodyCap = azureInflightBudget + 1 },
		"same host and route":  func(c *Config) { c.AzureGates = append(c.AzureGates, c.AzureGates[0]) },
		"rule without TLS":     func(c *Config) { c.Injection[0].RequireTLS = false },
		"rule without a pin":   func(c *Config) { c.Injection[0].PinRoutes = nil },
		"rule with a path pin": func(c *Config) { c.Injection[0].PinPath = "/p" },
		"rule wider than gate": func(c *Config) {
			c.Injection[0].PinRoutes = append(c.Injection[0].PinRoutes, egress.PinRoute{Method: http.MethodPost, Path: "/openai/files"})
		},
		"rule of the other route": func(c *Config) { c.Injection[0].PinRoutes = AzureRoutePins(types.AzureRouteOpenAIV1) },
	} {
		c := azureConfig(t)
		mutate(c)
		err := c.applyDefaultsAndValidate()
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "azure") {
			t.Errorf("%s: err = %v, want a refusal naming the Azure gate", name, err)
		}
	}
	// Two routes on one host are two gates; a rule on another host is not the gate's business.
	c := azureConfig(t)
	c.AzureGates = append(c.AzureGates, AzureGateConfig{Host: azHost, Route: types.AzureRouteOpenAIV1, Models: []string{azOpenAI}})
	c.Injection[0].PinRoutes = append(AzureRoutePins(types.AzureRouteAnthropic), AzureRoutePins(types.AzureRouteOpenAIV1)...)
	c.Injection = append(c.Injection, InjectionConfig{InjectionRule: egress.InjectionRule{Host: "other.test"}, GrantID: uuid.New()})
	if err := c.applyDefaultsAndValidate(); err != nil {
		t.Fatalf("two routes on one host: %v", err)
	}
}

// The key rides the config JSON, and a gate absent from it leaves the older shape unchanged.
func TestProxyConfig_AzureGatesRoundTrip(t *testing.T) {
	certPEM, keyPEM := genTestCA(t)
	cfg, err := LoadConfigBytes(baseConfigJSON(t, map[string]any{
		"mitm_ca_cert_pem": string(certPEM),
		"mitm_ca_key_pem":  string(keyPEM),
		"azure_gates":      []AzureGateConfig{{Host: azHost, Route: types.AzureRouteOpenAIV1, Models: []string{azOpenAI}, BodyCap: 1 << 20}},
	}))
	if err != nil {
		t.Fatalf("LoadConfigBytes: %v", err)
	}
	if len(cfg.AzureGates) != 1 || cfg.AzureGates[0].Models[0] != azOpenAI || cfg.AzureGates[0].BodyCap != 1<<20 {
		t.Fatalf("AzureGates = %+v", cfg.AzureGates)
	}
	plain, err := LoadConfigBytes(baseConfigJSON(t, nil))
	if err != nil || len(plain.AzureGates) != 0 {
		t.Fatalf("a config with no gate: %+v, %v", plain, err)
	}
}
