// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package spire_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"github.com/spiffe/go-spiffe/v2/spiffeid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/identitytest"
	"github.com/cjohnstoniv/wardyn/internal/identity/spire"
	"github.com/cjohnstoniv/wardyn/internal/identity/spire/spiretest"
	"github.com/cjohnstoniv/wardyn/internal/identity/svidsource"
)

func construct(t *testing.T, env map[string]string, cp []spire.Entry) error {
	t.Helper()
	srv, err := spiretest.NewServer(td, jose.ES256)
	if err != nil {
		t.Fatal(err)
	}
	srv.ControlPlane = cp
	d := identity.Deps{TrustDomain: td, Revocations: identitytest.NewMemRevocationStore(), Audit: &recorder{},
		Bindings: identitytest.NewMemBindingStore()}
	_, err = spire.ConstructForTest(d, spire.TestSeams{Env: env, Namespace: "wardyn", Server: srv,
		Open: func(context.Context, string, spiffeid.ID) (svidsource.Source, error) { return srv.Source(), nil }})
	return err
}

// S10: every constructor refusal, each naming what is wrong, and the accepted shape.
func TestSpire_ConstructorRefusals(t *testing.T) {
	good := []spire.Entry{controlPlaneEntry("k8s:ns:wardyn", "k8s:sa:wardyn")}
	sel := func(s string) map[string]string { e := goodEnv(); e["WARDYN_SPIRE_SELECTORS"] = s; return e }
	set := func(k, v string) map[string]string { e := goodEnv(); e[k] = v; return e }
	const comp, run = "k8s:pod-label:wardyn.component:proxy", "k8s:pod-label:wardyn.run-id:{run_id}"
	for _, tc := range []struct {
		name string
		env  map[string]string
		cp   []spire.Entry
		want string
	}{
		{"no_ns", sel("k8s:sa:wardyn-proxy," + comp + "," + run), good, "k8s:ns:"},
		{"no_sa", sel("k8s:ns:runs," + comp + "," + run), good, "k8s:sa:"},
		{"sa_default", sel("k8s:ns:runs,k8s:sa:default," + comp + "," + run), good, "k8s:sa:default"},
		{"no_component", sel("k8s:ns:runs,k8s:sa:wardyn-proxy," + run), good, "wardyn.component:proxy"},
		{"no_run_id", sel("k8s:ns:runs,k8s:sa:wardyn-proxy," + comp), good, "{run_id}"},
		{"docker", sel("k8s:ns:runs,k8s:sa:wardyn-proxy," + comp + "," + run + ",docker:label:a:b"), good, "docker:label:a:b"},
		{"ns_other", sel("k8s:ns:other,k8s:sa:wardyn-proxy," + comp + "," + run), good, "k8s:ns:other does not equal WARDYN_K8S_NAMESPACE (runs)"},
		{"ns_var_unset", set("WARDYN_K8S_NAMESPACE", ""), good, "k8s:ns:runs needs WARDYN_K8S_NAMESPACE"},
		{"sa_other", sel("k8s:ns:runs,k8s:sa:other-proxy," + comp + "," + run), good, "k8s:sa:other-proxy does not equal WARDYN_K8S_SPIFFE_PROXY_SERVICE_ACCOUNT (wardyn-proxy)"},
		{"sa_var_unset", set("WARDYN_K8S_SPIFFE_PROXY_SERVICE_ACCOUNT", ""), good, "k8s:sa:wardyn-proxy needs WARDYN_K8S_SPIFFE_PROXY_SERVICE_ACCOUNT"},
		{"socket_missing", set("WARDYN_SPIRE_AGENT_SOCKET", ""), good, "WARDYN_SPIRE_AGENT_SOCKET"},
		{"server_missing", set("WARDYN_SPIRE_SERVER_ADDRESS", ""), good, "WARDYN_SPIRE_SERVER_ADDRESS"},
		{"parent_missing", set("WARDYN_SPIRE_PARENT_ID", ""), good, "WARDYN_SPIRE_PARENT_ID"},
		{"pod_sa_unset", set("WARDYN_POD_SERVICE_ACCOUNT", ""), good, "WARDYN_POD_SERVICE_ACCOUNT"},
		{"pod_sa_default", set("WARDYN_POD_SERVICE_ACCOUNT", "default"), good, "WARDYN_POD_SERVICE_ACCOUNT is default"},
		{"pod_sa_is_proxy", set("WARDYN_POD_SERVICE_ACCOUNT", "wardyn-proxy"), good, "must differ"},
		{"cp_none", goodEnv(), nil, "no entry for wardynd's control-plane"},
		{"cp_ns_only", goodEnv(), []spire.Entry{controlPlaneEntry("k8s:ns:wardyn")}, "k8s:sa:wardyn"},
		{"cp_proxy_sa", goodEnv(), []spire.Entry{controlPlaneEntry("k8s:ns:wardyn", "k8s:sa:wardyn-proxy")}, "k8s:sa:wardyn"},
		{"cp_other_ns", goodEnv(), []spire.Entry{controlPlaneEntry("k8s:ns:runs", "k8s:sa:wardyn")}, "k8s:ns:wardyn"},
		{"cp_docker", goodEnv(), []spire.Entry{controlPlaneEntry("k8s:ns:wardyn", "k8s:sa:wardyn", "doc:label:x")}, "not a k8s one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := construct(t, tc.env, tc.cp)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("construct = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
	t.Run("accepted", func(t *testing.T) {
		if err := construct(t, goodEnv(), good); err != nil {
			t.Fatalf("construct with matching selectors and admin entry: %v", err)
		}
	})
}

// S10, the list-error case: a server that cannot list wardynd's entries refuses boot.
func TestSpire_ControlPlaneListErrorRefuses(t *testing.T) {
	srv, _ := spiretest.NewServer(td, jose.ES256)
	srv.SetDown(true)
	d := identity.Deps{TrustDomain: td, Revocations: identitytest.NewMemRevocationStore(), Audit: &recorder{},
		Bindings: identitytest.NewMemBindingStore()}
	_, err := spire.ConstructForTest(d, spire.TestSeams{Env: goodEnv(), Namespace: "wardyn", Server: srv,
		Open: func(context.Context, string, spiffeid.ID) (svidsource.Source, error) { return srv.Source(), nil }})
	if err == nil || !strings.Contains(err.Error(), "WARDYN_SPIRE_SERVER_ADDRESS") {
		t.Fatalf("construct with a failing entry list = %v, want a refusal naming the server", err)
	}
}

// S17: a Workload API socket that accepts and never answers, and an entry list that never answers, each
// refuse within the boot wait (or the BootCtx's shorter deadline) plus 1 s, naming the socket or server.
func TestSpire_BootWaitIsBounded(t *testing.T) {
	dir, err := os.MkdirTemp("", "spt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()
	env := goodEnv()
	env["WARDYN_SPIRE_AGENT_SOCKET"] = "unix://" + sock
	srv, _ := spiretest.NewServer(td, jose.ES256)
	deps := func(ctx context.Context) identity.Deps {
		return identity.Deps{TrustDomain: td, Revocations: identitytest.NewMemRevocationStore(), Audit: &recorder{},
			Bindings: identitytest.NewMemBindingStore(), BootCtx: ctx}
	}
	bounded := func(name string, ctx context.Context, s spire.TestSeams, bound time.Duration, want string) {
		start := time.Now()
		p, err := spire.ConstructForTest(deps(ctx), s)
		if took := time.Since(start); p != nil || err == nil || !strings.Contains(err.Error(), want) || took > bound+time.Second {
			t.Errorf("%s: construct = (%v, %v) after %v, want a refusal naming %q within %v", name, p, err, took, want, bound+time.Second)
		}
	}
	bounded("socket", context.Background(), spire.TestSeams{Env: env, Namespace: "wardyn", Server: srv},
		spire.SpireBootWait, "did not answer within 10s (WARDYN_SPIRE_AGENT_SOCKET=unix://"+sock)
	short, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	bounded("socket_short_bootctx", short, spire.TestSeams{Env: env, Namespace: "wardyn", Server: srv},
		2*time.Second, "WARDYN_SPIRE_AGENT_SOCKET=unix://"+sock)
	srv.SetBlockList(true)
	bounded("entry_list", context.Background(), spire.TestSeams{Env: goodEnv(), Namespace: "wardyn", Server: srv,
		Open: func(context.Context, string, spiffeid.ID) (svidsource.Source, error) { return srv.Source(), nil }},
		spire.SpireBootWait, "did not answer within 10s (WARDYN_SPIRE_SERVER_ADDRESS=spire-server.spire:8081)")
}

func b64(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

// S15: alg none, HS256 over the bundle key's bytes and EdDSA are refused at parse; one token per
// accepted family verifies; an expired token whose sub is not a run path gets a flat error.
func TestSpire_AlgorithmsAndFlatExpiry(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, identitytest.NewMemRevocationStore(), jose.ES256)
	now := time.Now()
	claims := jwt.Claims{Subject: "spiffe://wardyn.local/agent-run/" + uuid.NewString(), Audience: jwt.Audience{aud},
		IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(time.Hour))}
	none := b64(map[string]string{"alg": "none", "kid": spiretest.KeyID, "typ": "JWT"}) + "." + b64(claims) + "."
	pub, _ := x509.MarshalPKIXPublicKey(h.srv.Key().Public())
	_, edKey, _ := ed25519.GenerateKey(rand.Reader)
	forged := []string{none}
	for _, k := range []jose.SigningKey{{Algorithm: jose.HS256, Key: pub}, {Algorithm: jose.EdDSA, Key: edKey}} {
		signer, err := jose.NewSigner(k, (&jose.SignerOptions{}).WithHeader("kid", spiretest.KeyID))
		if err != nil {
			t.Fatal(err)
		}
		tok, err := jwt.Signed(signer).Claims(claims).Serialize()
		if err != nil {
			t.Fatal(err)
		}
		forged = append(forged, tok)
	}
	for i, tok := range forged {
		if _, err := h.p.Verify(ctx, tok, aud); err == nil || !strings.Contains(err.Error(), "parse token") {
			t.Errorf("forged token %d = %v, want a refusal at parse", i, err)
		}
	}
	for _, alg := range []jose.SignatureAlgorithm{jose.RS256, jose.ES256, jose.PS256} {
		a := newHarness(t, identitytest.NewMemRevocationStore(), alg)
		ri, err := a.p.MintRunIdentity(ctx, uuid.New(), "alice", "", aud, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.p.Verify(ctx, ri.Token, aud); err != nil {
			t.Errorf("%s: verify: %v", alg, err)
		}
	}
	old, err := h.srv.Sign("spiffe://wardyn.local/wardyn/control-plane", aud, now.Add(-3*time.Hour), now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var exp *identity.ExpiredTokenError
	if _, err := h.p.Verify(ctx, old, aud); err == nil || errors.As(err, &exp) {
		t.Fatalf("an expired non-run token = %v, want a flat error naming no run", err)
	}
}
