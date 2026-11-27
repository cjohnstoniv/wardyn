// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/embedded"
	"github.com/cjohnstoniv/wardyn/internal/identity/identitytest"
	"github.com/cjohnstoniv/wardyn/internal/identity/svidsource"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mtlsSource is a SPIRE source that holds wardynd's own SVID and nothing else.
type mtlsSource struct{ own spiffeid.ID }

func (s mtlsSource) GetX509SVID() (*x509svid.SVID, error) { return &x509svid.SVID{ID: s.own}, nil }

func (mtlsSource) GetX509BundleForTrustDomain(spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	return nil, errors.New("no bundle in this test")
}

func (mtlsSource) GetJWTBundleForTrustDomain(spiffeid.TrustDomain) (*jwtbundle.Bundle, error) {
	return nil, errors.New("no bundle in this test")
}

func (mtlsSource) Close() error { return nil }

// mtlsHolder is a provider that holds a SPIRE source, as spire does.
type mtlsHolder struct {
	identity.Provider
	src svidsource.Source
}

func (h mtlsHolder) SVIDSource() svidsource.Source { return h.src }

type mtlsNopRecorder struct{}

func (mtlsNopRecorder) Record(context.Context, types.AuditEvent) error { return nil }

// The switch is off unless set: with no variable and no flag, and with an empty variable, it reads false,
// and ENV.md documents false.
func TestBootFlags_InternalMTLSDefaultOff(t *testing.T) {
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	for _, c := range []struct {
		name string
		env  *string
		want bool
	}{
		{"unset", nil, false},
		{"empty", new(string), false},
		{"on", ptr("true"), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ensureUnset(t, "WARDYN_INTERNAL_MTLS")
			if c.env != nil {
				t.Setenv("WARDYN_INTERNAL_MTLS", *c.env)
			}
			os.Args = []string{"wardynd-test"}
			resetFlags(t)
			if got := *parseBootFlags().internalMTLS; got != c.want {
				t.Fatalf("WARDYN_INTERNAL_MTLS (%s) reads %v, want %v", c.name, got, c.want)
			}
		})
	}
	doc, err := os.ReadFile("../../docs/ENV.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "\n| `WARDYN_INTERNAL_MTLS` | bool | `false` |") {
		t.Error("docs/ENV.md has no WARDYN_INTERNAL_MTLS row documenting the default false")
	}
}

// Boot refuses the switch on unless the internal listener binds, the provider holds a SPIRE source and
// WARDYN_RUNNER is k8s; each refusal names what is missing. The runner flag decides, not the runner target
// a test harness can set on its own. Off is nil whatever else is set.
func TestLoadInternalMTLS_Refusals(t *testing.T) {
	emb, err := embedded.New(nil, "wardyn.local", identitytest.NewMemRevocationStore(), mtlsNopRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	src := mtlsSource{own: spiffeid.RequireFromString("spiffe://example.org" + identity.ControlPlanePath)}
	spire := mtlsHolder{Provider: emb, src: src}
	const https = "https://wardynd:8443"
	for _, tc := range []struct {
		name, url, runner, target, want string
		prov                            identity.Provider
	}{
		{name: "the embedded provider", prov: emb, url: https, runner: "k8s", want: "WARDYN_IDENTITY=spire"},
		{name: "a loopback http control-plane URL", prov: spire, url: "http://127.0.0.1:8080", runner: "k8s", want: "an https WARDYN_CONTROL_PLANE_URL"},
		{name: "the docker runner", prov: spire, url: https, runner: "docker", want: "WARDYN_RUNNER=k8s"},
		{name: "no runner with a k8s runner target", prov: spire, url: https, runner: "none", target: "k8s", want: "WARDYN_RUNNER=k8s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WARDYN_RUNNER_TARGET", tc.target)
			m, err := loadInternalMTLS(true, tc.prov, tc.url, tc.runner)
			if m != nil || err == nil || !strings.HasPrefix(err.Error(), "refusing to start: WARDYN_INTERNAL_MTLS needs") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("loadInternalMTLS = %v, %v; want a refusal naming %q", m, err, tc.want)
			}
		})
	}
	for _, prov := range []identity.Provider{emb, spire} {
		if m, err := loadInternalMTLS(false, prov, "http://127.0.0.1:8080", "none"); m != nil || err != nil {
			t.Fatalf("off: loadInternalMTLS = %v, %v; want nil, nil", m, err)
		}
	}
	m, err := loadInternalMTLS(true, spire, https, "k8s")
	if err != nil {
		t.Fatalf("spire, https and k8s: %v", err)
	}
	if m == nil || m.TrustDomain != "example.org" || m.Bundles != svidsource.Source(src) {
		t.Fatalf("loadInternalMTLS = %+v, want the provider's source and its trust domain example.org", m)
	}
}

// The internal listener asks for client certificates only with the switch on, never changes the shared
// hop config, and the relay listener, which serves from that config, never asks.
func TestStartInternalListener_ClientAuthFollowsSwitch(t *testing.T) {
	hop, err := loadHopTLS(context.Background(), unlocked(mapKeyStore{}), "https://wardynd:8443")
	if err != nil {
		t.Fatal(err)
	}
	off, on := false, true
	for _, tc := range []struct {
		name string
		flag *bool
		want tls.ClientAuthType
	}{
		{"flag not set", nil, tls.NoClientCert},
		{"off", &off, tls.NoClientCert},
		{"on", &on, tls.RequestClientCert},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := "127.0.0.1:0"
			srv := startInternalListener(hop, &bootFlags{internalListen: &addr, internalMTLS: tc.flag}, http.NotFoundHandler(), make(chan error, 1))
			defer srv.Close()
			if srv.TLSConfig.ClientAuth != tc.want {
				t.Fatalf("internal listener ClientAuth = %v, want %v", srv.TLSConfig.ClientAuth, tc.want)
			}
		})
	}
	if hop.server.ClientAuth != tls.NoClientCert {
		t.Fatalf("the shared hop config was changed: ClientAuth = %v", hop.server.ClientAuth)
	}
	relay := startRelayListener(hop, api.New(api.Config{}))
	defer relay.Close()
	if relay.TLSConfig.ClientAuth != tls.NoClientCert {
		t.Fatalf("relay listener ClientAuth = %v, want %v", relay.TLSConfig.ClientAuth, tls.NoClientCert)
	}
}
