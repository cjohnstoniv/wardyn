// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/embedded"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The run-certificate check on the internal surface (internal_peer.go). Every key and certificate here is made
// in the test; none is a fixture file.

const (
	mtlsDecisionBody  = `{"request":{"host":"api.anthropic.com","method":"POST"},"decision":"allow"}`
	mtlsTrustDomain   = "wardyn.local"
	mtlsSubjectMarker = "mtls-leaf-subject-marker"
)

func mtlsKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// mtlsCA is a certificate authority made in the test: a SPIRE trust bundle for one trust domain, or the
// internal CA that signs the listener's serving certificate.
type mtlsCA struct {
	td   spiffeid.TrustDomain
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newMTLSCA(t *testing.T, td string) *mtlsCA {
	t.Helper()
	key := mtlsKey(t)
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test authority " + td},
		NotBefore:             now.Add(-24 * time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &mtlsCA{td: spiffeid.RequireTrustDomainFromString(td), cert: cert, key: key}
}

func (c *mtlsCA) bundle() *x509bundle.Bundle {
	return x509bundle.FromX509Authorities(c.td, []*x509.Certificate{c.cert})
}

// mtlsLeaf is an X.509-SVID for id, valid from notBefore to notAfter, signed by ca; a nil ca self-signs it.
func mtlsLeaf(t *testing.T, ca *mtlsCA, id string, notBefore, notAfter time.Time) tls.Certificate {
	t.Helper()
	key := mtlsKey(t)
	u, err := url.Parse(id)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: mtlsSubjectMarker},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{u},
	}
	parent, signer := tmpl, key
	if ca != nil {
		parent, signer = ca.cert, ca.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

func mtlsState(c tls.Certificate) *tls.ConnectionState {
	return &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, PeerCertificates: []*x509.Certificate{c.Leaf}}
}

// mtlsBundles counts the bundle lookups x509svid.Verify makes, so a test can tell whether refuseUnverifiedPeer
// got as far as verifying a certificate.
type mtlsBundles struct {
	x509bundle.Source
	n atomic.Int32
}

func (b *mtlsBundles) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	b.n.Add(1)
	return b.Source.GetX509BundleForTrustDomain(td)
}

// mtlsRunStore serves one run as the store holds it, with no runner action pending.
type mtlsRunStore struct {
	*touchStore
	store.RunnerStore
	run types.AgentRun
}

func (s mtlsRunStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	r := s.run
	r.ID = id
	return r, nil
}

func (mtlsRunStore) RunHasPendingRunnerAction(context.Context, uuid.UUID) (bool, error) {
	return false, nil
}

// mtlsExpiringIDP is the harness's provider, except that one token reads as expired.
type mtlsExpiringIDP struct {
	*embedded.Provider
	expired string
	runID   uuid.UUID
}

func (p mtlsExpiringIDP) Verify(ctx context.Context, token, audience string) (*identity.Claims, error) {
	if token == p.expired {
		return nil, &identity.ExpiredTokenError{RunID: p.runID, Expiry: time.Now().Add(-time.Hour), Err: errors.New("token is expired")}
	}
	return p.Provider.Verify(ctx, token, audience)
}

// mtlsFixture is one server and one run A, with its token and its SPIFFE ID as the provider minted them.
type mtlsFixture struct {
	h       *harness
	srv     *Server
	ca      *mtlsCA
	bundles *mtlsBundles
	runID   uuid.UUID
	id      string
	tok     string
	now     time.Time
	clock   *atomic.Int64
}

// newMTLSFixture builds the server. on sets InternalMTLS over ca's bundle and every extra one; edit, when not
// nil, changes the Config before New.
func newMTLSFixture(t *testing.T, st store.Store, on bool, edit func(*Config), extra ...*mtlsCA) *mtlsFixture {
	t.Helper()
	h := newHarness(t)
	ca := newMTLSCA(t, mtlsTrustDomain)
	runID := uuid.New()
	ri, err := h.idp.MintRunIdentity(context.Background(), runID, "alice@example.com", "", internalAudience, false)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	clock := &atomic.Int64{}
	clock.Store(now.UnixNano())
	sets := []*x509bundle.Bundle{ca.bundle()}
	for _, x := range extra {
		sets = append(sets, x.bundle())
	}
	b := &mtlsBundles{Source: x509bundle.NewSet(sets...)}
	cfg := baseTestConfig(h, st)
	cfg.Now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	if on {
		cfg.InternalMTLS = &InternalMTLS{Bundles: b, TrustDomain: mtlsTrustDomain}
	}
	if edit != nil {
		edit(&cfg)
	}
	return &mtlsFixture{h: h, srv: New(cfg), ca: ca, bundles: b, runID: runID, id: ri.SPIFFEID, tok: ri.Token, now: now, clock: clock}
}

func (f *mtlsFixture) setNow(at time.Time) { f.clock.Store(at.UnixNano()) }

// leaf is a run certificate for id from f's bundle, valid around f.now.
func (f *mtlsFixture) leaf(t *testing.T, id string) tls.Certificate {
	t.Helper()
	return mtlsLeaf(t, f.ca, id, f.now.Add(-time.Minute), f.now.Add(time.Hour))
}

// postWithPeer sends one request to the internal router as it arrives off the listener: cs is the
// connection's TLS state (nil for none), and ctx, when not nil, adds what a relay or the in-process via_org
// transport adds.
func postWithPeer(t *testing.T, srv *Server, path, body, bearer string, cs *tls.ConnectionState, ctx func(context.Context) context.Context) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	r.TLS = cs
	if ctx != nil {
		r = r.WithContext(ctx(r.Context()))
	}
	w := httptest.NewRecorder()
	panicFails(t, srv.Handler()).ServeHTTP(deadlineRecorder{w}, r)
	return w
}

func mtlsDecision(t *testing.T, f *mtlsFixture, bearer string, cs *tls.ConnectionState, ctx func(context.Context) context.Context) *httptest.ResponseRecorder {
	t.Helper()
	return postWithPeer(t, f.srv, "/api/v1/internal/decisions", mtlsDecisionBody, bearer, cs, ctx)
}

// mtlsReason is the reason of an error body, "" for any other body.
func mtlsReason(b []byte) string {
	var e errorBody
	_ = json.Unmarshal(b, &e)
	return e.Reason
}

func authFailedWith(h *harness, reason string) bool {
	for _, ev := range h.audit.snapshot() {
		if ev.Action != "auth.fail" || ev.Actor != internalAuthActor {
			continue
		}
		var d struct {
			Reason string `json:"reason"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.Reason == reason {
			return true
		}
	}
	return false
}

func anyPeerRefusalAudited(h *harness) bool {
	for _, r := range []string{reasonInternalPeerRequired, reasonInternalPeerInvalid, reasonInternalPeerNotARun, reasonInternalPeerMismatch} {
		if authFailedWith(h, r) {
			return true
		}
	}
	return false
}

// wantMTLSRefusal: the request was refused with code and reason, the handler did not answer, and the
// refusal has its auth.fail row.
func wantMTLSRefusal(t *testing.T, f *mtlsFixture, w *httptest.ResponseRecorder, code int, reason string) {
	t.Helper()
	if w.Code != code || mtlsReason(w.Body.Bytes()) != reason {
		t.Fatalf("answered %d %q, want %d %s; body=%s", w.Code, mtlsReason(w.Body.Bytes()), code, reason, w.Body.String())
	}
	if !authFailedWith(f.h, reason) {
		t.Errorf("no auth.fail row by %s with reason %s", internalAuthActor, reason)
	}
}

func wantServed(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusAccepted {
		t.Fatalf("answered %d, want 202 served; body=%s", w.Code, w.Body.String())
	}
}

// Off is the release before it, byte for byte: no TLS state or any certificate at all is served, and the
// response says nothing about the connection.
func TestInternalMTLS_OffIsUnchanged(t *testing.T) {
	f := newMTLSFixture(t, &touchStore{}, false, nil)
	if f.srv.cfg.InternalMTLS != nil {
		t.Fatal("the fixture turned the switch on")
	}
	stranger := mtlsLeaf(t, nil, "spiffe://"+mtlsTrustDomain+"/wardyn/control-plane", f.now.Add(-time.Minute), f.now.Add(time.Second))
	for name, cs := range map[string]*tls.ConnectionState{
		"no TLS state":                              nil,
		"TLS with no certificate":                   {Version: tls.VersionTLS13},
		"an unverifiable certificate in its window": mtlsState(stranger),
	} {
		t.Run(name, func(t *testing.T) {
			w := mtlsDecision(t, f, f.tok, cs, nil)
			wantServed(t, w)
			if got := w.Header().Get("Connection"); got != "" {
				t.Errorf("Connection = %q with the switch off, want none", got)
			}
		})
	}
	if n := f.bundles.n.Load(); n != 0 {
		t.Errorf("the bundle was read %d times with the switch off", n)
	}
}

// On, a request that did not come through a relay and has no verified run certificate fails closed, with its
// audit row, whatever else it carries; the clock is the server's (Config.Now), not the wall's.
func TestInternalMTLS_NoPeerOrUnverifiedPeerRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cs     func(t *testing.T, f *mtlsFixture) *tls.ConnectionState
		reason string
	}{
		{"no TLS state", func(*testing.T, *mtlsFixture) *tls.ConnectionState { return nil }, reasonInternalPeerRequired},
		{"TLS with no certificate", func(*testing.T, *mtlsFixture) *tls.ConnectionState {
			return &tls.ConnectionState{Version: tls.VersionTLS13}
		}, reasonInternalPeerRequired},
		{"a self-signed certificate with the run's URI", func(t *testing.T, f *mtlsFixture) *tls.ConnectionState {
			return mtlsState(mtlsLeaf(t, nil, f.id, f.now.Add(-time.Minute), f.now.Add(time.Hour)))
		}, reasonInternalPeerInvalid},
		{"an expired certificate", func(t *testing.T, f *mtlsFixture) *tls.ConnectionState {
			return mtlsState(mtlsLeaf(t, f.ca, f.id, f.now.Add(-time.Hour), f.now.Add(-time.Second)))
		}, reasonInternalPeerInvalid},
		{"a certificate not yet valid", func(t *testing.T, f *mtlsFixture) *tls.ConnectionState {
			return mtlsState(mtlsLeaf(t, f.ca, f.id, f.now.Add(time.Second), f.now.Add(time.Hour)))
		}, reasonInternalPeerInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &touchStore{}
			f := newMTLSFixture(t, st, true, nil)
			w := mtlsDecision(t, f, f.tok, tc.cs(t, f), nil)
			wantMTLSRefusal(t, f, w, http.StatusUnauthorized, tc.reason)
			if len(st.touched) != 0 {
				t.Errorf("the decision handler ran for a refused request: touched %v", st.touched)
			}
			if wantClose := tc.reason == reasonInternalPeerInvalid; (w.Header().Get("Connection") == "close") != wantClose {
				t.Errorf("Connection = %q, want close only for %s", w.Header().Get("Connection"), reasonInternalPeerInvalid)
			}
		})
	}
	// The same certificate the expired case refuses is served once the server's clock is inside its window.
	f := newMTLSFixture(t, &touchStore{}, true, nil)
	old := mtlsLeaf(t, f.ca, f.id, f.now.Add(-2*time.Hour), f.now.Add(-time.Hour))
	f.setNow(f.now.Add(-90 * time.Minute))
	wantServed(t, mtlsDecision(t, f, f.tok, mtlsState(old), nil))
}

// The certificate never stands in for the bearer: with a valid run-A certificate, a missing, denylisted or
// expired token, and a token of a run that is over, are each refused by the bearer's own checks, before the
// certificate is ever verified.
func TestInternalMTLS_BearerStillChecked(t *testing.T) {
	over := func(runID uuid.UUID) store.Store {
		return terminalRunStore{runID: runID, state: types.RunKilled, age: terminalUploadGrace + time.Minute}
	}
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T) (*mtlsFixture, string)
		code   int
		reason string
	}{
		{"no bearer", func(t *testing.T) (*mtlsFixture, string) {
			return newMTLSFixture(t, &touchStore{}, true, nil), ""
		}, http.StatusUnauthorized, reasonMissingRunToken},
		{"a denylisted bearer", func(t *testing.T) (*mtlsFixture, string) {
			f := newMTLSFixture(t, &touchStore{}, true, nil)
			if err := f.h.idp.RevokeRun(context.Background(), f.runID); err != nil {
				t.Fatal(err)
			}
			return f, f.tok
		}, http.StatusUnauthorized, ""},
		{"an expired bearer", func(t *testing.T) (*mtlsFixture, string) {
			f := newMTLSFixture(t, &touchStore{}, true, func(c *Config) {
				c.Identity = mtlsExpiringIDP{Provider: c.Identity.(*embedded.Provider), expired: "expired-run-token", runID: uuid.New()}
			})
			return f, "expired-run-token"
		}, http.StatusUnauthorized, ""},
		{"a run that is over", func(t *testing.T) (*mtlsFixture, string) {
			runID := uuid.New()
			f := newMTLSFixture(t, over(runID), true, nil)
			tok := f.h.mintRunToken(t, runID)
			f.id = "spiffe://" + mtlsTrustDomain + "/agent-run/" + runID.String()
			return f, tok
		}, http.StatusForbidden, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, bearer := tc.setup(t)
			w := mtlsDecision(t, f, bearer, mtlsState(f.leaf(t, f.id)), nil)
			if w.Code != tc.code || (tc.reason != "" && mtlsReason(w.Body.Bytes()) != tc.reason) {
				t.Fatalf("answered %d %q, want %d %s; body=%s", w.Code, mtlsReason(w.Body.Bytes()), tc.code, tc.reason, w.Body.String())
			}
			if strings.HasPrefix(mtlsReason(w.Body.Bytes()), "internal_peer_") || anyPeerRefusalAudited(f.h) {
				t.Errorf("the certificate check answered a request the bearer refuses: %s", w.Body.String())
			}
			if n := f.bundles.n.Load(); n != 0 {
				t.Errorf("the certificate was verified (%d bundle reads) for a request the bearer refuses", n)
			}
		})
	}
	// The control: the same certificate with run A's live bearer reaches the check and is served.
	f := newMTLSFixture(t, &touchStore{}, true, nil)
	wantServed(t, mtlsDecision(t, f, f.tok, mtlsState(f.leaf(t, f.id)), nil))
	if f.bundles.n.Load() == 0 {
		t.Fatal("a served request never read the bundle: the counter above proves nothing")
	}
}

// Another run's certificate is refused, and so is run A's own run id under another path.
func TestInternalMTLS_OtherRunsCertificateRefused(t *testing.T) {
	for name, id := range map[string]func(f *mtlsFixture) string{
		"run B's certificate": func(*mtlsFixture) string {
			return "spiffe://" + mtlsTrustDomain + "/agent-run/" + uuid.NewString()
		},
		"run A's id under a workload path": func(f *mtlsFixture) string {
			return "spiffe://" + mtlsTrustDomain + "/workload/alice/build/run/" + f.runID.String()
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMTLSFixture(t, &touchStore{}, true, nil)
			w := mtlsDecision(t, f, f.tok, mtlsState(f.leaf(t, id(f))), nil)
			wantMTLSRefusal(t, f, w, http.StatusForbidden, reasonInternalPeerMismatch)
		})
	}
}

// A SPIFFE ID that is not a run's is refused even when it verifies: wardynd's own, the chart's
// namespace/service-account fallback, a run path with no run id, and a run path in another trust domain whose
// bundle the server holds.
func TestInternalMTLS_NonRunIDRefused(t *testing.T) {
	foreign := newMTLSCA(t, "other.example")
	for _, tc := range []struct {
		name string
		ca   func(f *mtlsFixture) *mtlsCA
		id   func(f *mtlsFixture) string
	}{
		{"wardynd's own ID", func(f *mtlsFixture) *mtlsCA { return f.ca }, func(*mtlsFixture) string {
			return "spiffe://" + mtlsTrustDomain + identity.ControlPlanePath
		}},
		{"the namespace and service-account fallback", func(f *mtlsFixture) *mtlsCA { return f.ca }, func(*mtlsFixture) string {
			return "spiffe://" + mtlsTrustDomain + "/ns/wardyn-runs/sa/wardyn-proxy"
		}},
		{"a run path with no run id", func(f *mtlsFixture) *mtlsCA { return f.ca }, func(*mtlsFixture) string {
			return "spiffe://" + mtlsTrustDomain + "/agent-run/not-a-uuid"
		}},
		{"run A's path in another trust domain", func(*mtlsFixture) *mtlsCA { return foreign }, func(f *mtlsFixture) string {
			return "spiffe://other.example/agent-run/" + f.runID.String()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMTLSFixture(t, &touchStore{}, true, nil, foreign)
			leaf := mtlsLeaf(t, tc.ca(f), tc.id(f), f.now.Add(-time.Minute), f.now.Add(time.Hour))
			w := mtlsDecision(t, f, f.tok, mtlsState(leaf), nil)
			wantMTLSRefusal(t, f, w, http.StatusForbidden, reasonInternalPeerNotARun)
		})
	}
}

// The bearer runs first: in internalAuth, the token's verification, the liveness gate and the relay binding
// each come before the certificate check; that check is called once in this package, from internalAuth, so
// every group that mounts internalAuth gets it.
func TestInternalAuth_PeerCheckAfterBearer(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var calls []string
	pos := map[string]token.Pos{}
	mounts := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, d := range file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			inAuth := fd.Recv != nil && fd.Name.Name == "internalAuth"
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "s" && sel.Sel.Name == "internalAuth" {
					mounts++
				}
				switch sel.Sel.Name {
				case "refuseUnverifiedPeer":
					calls = append(calls, fmt.Sprintf("%s:%s", fset.Position(sel.Pos()), fd.Name.Name))
					fallthrough
				case "verifyRunToken", "refuseTerminalRun", "refuseRelayBinding":
					if inAuth {
						pos[sel.Sel.Name] = sel.Pos()
					}
				}
				return true
			})
		}
	}
	if len(calls) != 1 || !strings.HasSuffix(calls[0], ":internalAuth") {
		t.Fatalf("refuseUnverifiedPeer is used at %v, want exactly once, inside internalAuth", calls)
	}
	for _, before := range []string{"verifyRunToken", "refuseTerminalRun", "refuseRelayBinding"} {
		if pos[before] == token.NoPos || pos[before] >= pos["refuseUnverifiedPeer"] {
			t.Errorf("%s does not come before refuseUnverifiedPeer in internalAuth", before)
		}
	}
	if mounts < 2 {
		t.Errorf("found %d groups mounting s.internalAuth, want the internal routes and the gateway-identity door", mounts)
	}
}

// Relayed and in-process requests are bound to their runner instead, as before: a local run through its
// runner's relay, and the via_org engine's own call, need no certificate, and a relay of the wrong runner is
// still refused by the relay binding.
func TestInternalMTLS_RelayedAndInProcessUnchanged(t *testing.T) {
	runner := uuid.New()
	local := func() store.Store {
		return mtlsRunStore{touchStore: &touchStore{}, run: types.AgentRun{State: types.RunRunning, Placement: types.PlacementLocal, RunnerID: &runner, UpdatedAt: time.Now().UTC()}}
	}
	relayed := func(by uuid.UUID) func(context.Context) context.Context {
		return func(ctx context.Context) context.Context { return relayConnContext(ctx, relayedConn{runner: by}) }
	}
	viaOrg := func(ctx context.Context) context.Context {
		return context.WithValue(context.WithValue(ctx, viaOrgCallKey{}, true), relayRunnerKey{}, runner)
	}
	for name, ctx := range map[string]func(context.Context) context.Context{
		"through the runner's relay":    relayed(runner),
		"the via_org engine's own call": viaOrg,
	} {
		t.Run(name, func(t *testing.T) {
			f := newMTLSFixture(t, local(), true, nil)
			wantServed(t, mtlsDecision(t, f, f.tok, nil, ctx))
			if n := f.bundles.n.Load(); n != 0 || anyPeerRefusalAudited(f.h) {
				t.Errorf("a relayed request met the certificate check (%d bundle reads)", n)
			}
		})
	}
	t.Run("a relay of another runner", func(t *testing.T) {
		f := newMTLSFixture(t, local(), true, nil)
		w := mtlsDecision(t, f, f.tok, nil, relayed(uuid.New()))
		if w.Code != http.StatusForbidden || mtlsReason(w.Body.Bytes()) != string(placement.ReasonRelayRunnerMismatch) {
			t.Fatalf("answered %d %s, want 403 %s", w.Code, w.Body.String(), placement.ReasonRelayRunnerMismatch)
		}
	})
}

// The host sensor's ground-truth door has its own audience and middleware; the switch does not reach it.
func TestInternalMTLS_GroundtruthUnchanged(t *testing.T) {
	f := newMTLSFixture(t, nil, true, nil)
	w := postWithPeer(t, f.srv, "/api/v1/internal/groundtruth", `{"events":[]}`, f.h.mintGroundtruthToken(t), nil, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("ground-truth batch with no client certificate answered %d, want 202; body=%s", w.Code, w.Body.String())
	}
}

// mtlsTLSServer serves srv's router as the internal listener does with the switch on: a serving certificate
// from an internal CA, TLS 1.3, and RequestClientCert. It returns the pool a proxy pins.
func mtlsTLSServer(t *testing.T, srv *Server, h2 bool) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	ca := newMTLSCA(t, "internal-ca.test")
	key := mtlsKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "wardynd"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(panicFails(t, srv.Handler()))
	ts.EnableHTTP2 = h2
	ts.TLS = &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		ClientAuth:   tls.RequestClientCert,
		MinVersion:   tls.VersionTLS13,
	}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return ts, pool
}

// mtlsClient is a proxy's control-plane client: it pins pool and presents leaf when one is given.
func mtlsClient(pool *x509.CertPool, leaf *tls.Certificate, h2 bool) *http.Client {
	cfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}
	if leaf != nil {
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return leaf, nil }
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, ForceAttemptHTTP2: h2}, Timeout: 10 * time.Second}
}

type mtlsResp struct {
	code      int
	reason    string
	connClose bool // the HTTP/1.1 response said Connection: close
	reused    bool // the request went out on a connection already open
	proto     int
}

func mtlsPost(t *testing.T, c *http.Client, base, tok string) mtlsResp {
	t.Helper()
	var reused bool
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/internal/decisions", strings.NewReader(mtlsDecisionBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(i httptrace.GotConnInfo) { reused = i.Reused },
	}))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return mtlsResp{code: resp.StatusCode, reason: mtlsReason(body), connClose: resp.Close || strings.EqualFold(resp.Header.Get("Connection"), "close"),
		reused: reused, proto: resp.ProtoMajor}
}

// Over a real handshake: a proxy presenting its run's SVID is served and one presenting none is refused.
// Keep-alive holds while the leaf is outside its window (40% of its life), and the connection is closed once
// it is inside (25 s left of 120 s, 140 s left of 10 min), so the next request re-dials. Over HTTP/2 the
// response carries no Connection header, so the close is read from the next request's connection.
func TestInternalMTLS_RealHandshake(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		for _, tc := range []struct {
			ttl, left time.Duration
		}{{120 * time.Second, 25 * time.Second}, {10 * time.Minute, 140 * time.Second}} {
			t.Run(fmt.Sprintf("http2=%v/%s", h2, tc.ttl), func(t *testing.T) {
				f := newMTLSFixture(t, &touchStore{}, true, nil)
				ts, pool := mtlsTLSServer(t, f.srv, h2)
				wantProto := 1
				if h2 {
					wantProto = 2
				}

				r := mtlsPost(t, mtlsClient(pool, nil, h2), ts.URL, f.tok)
				if r.code != http.StatusUnauthorized || r.reason != reasonInternalPeerRequired {
					t.Fatalf("no client certificate answered %d %q, want 401 %s", r.code, r.reason, reasonInternalPeerRequired)
				}

				leaf := mtlsLeaf(t, f.ca, f.id, f.now, f.now.Add(tc.ttl))
				c := mtlsClient(pool, &leaf, h2)
				f.setNow(f.now.Add(tc.ttl * 4 / 10))
				first := mtlsPost(t, c, ts.URL, f.tok)
				if first.code != http.StatusAccepted || first.proto != wantProto || first.connClose {
					t.Fatalf("at 40%% of the leaf's life: %+v, want 202 over HTTP/%d with keep-alive", first, wantProto)
				}
				if second := mtlsPost(t, c, ts.URL, f.tok); second.code != http.StatusAccepted || !second.reused {
					t.Fatalf("the next request at 40%%: %+v, want 202 on the same connection", second)
				}

				f.setNow(f.now.Add(tc.ttl - tc.left))
				near := mtlsPost(t, c, ts.URL, f.tok)
				if near.code != http.StatusAccepted || !near.reused {
					t.Fatalf("inside the window: %+v, want 202 on the kept connection", near)
				}
				if !h2 && !near.connClose {
					t.Errorf("inside the window over HTTP/1.1 the response has no Connection: close")
				}
				if after := mtlsPost(t, c, ts.URL, f.tok); after.code != http.StatusAccepted || after.reused {
					t.Fatalf("the request after a response inside the window: %+v, want 202 on a new connection", after)
				}
			})
		}
	}
}

// The window is a quarter of the leaf's lifetime, at least 15 s, at most 10 minutes, and never more than half.
func TestPeerExpiryWindow(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	leaf := func(ttl time.Duration) *x509.Certificate {
		return &x509.Certificate{NotBefore: at, NotAfter: at.Add(ttl)}
	}
	for ttl, want := range map[time.Duration]time.Duration{
		10 * time.Minute:  150 * time.Second,
		120 * time.Second: 30 * time.Second,
		40 * time.Second:  15 * time.Second,
		20 * time.Second:  10 * time.Second,
		time.Hour:         10 * time.Minute,
	} {
		if got := peerExpiryWindow(leaf(ttl)); got != want {
			t.Errorf("window of a %s leaf = %s, want %s", ttl, got, want)
		}
	}
	for _, ttl := range []time.Duration{0, -time.Second} {
		if !peerNearExpiry(leaf(ttl), at.Add(-time.Hour)) {
			t.Errorf("a leaf with lifetime %s is not inside its window", ttl)
		}
	}
	for ttl := time.Second; ttl <= 2*time.Hour; ttl += time.Second {
		if w := peerExpiryWindow(leaf(ttl)); w > ttl/2 {
			t.Fatalf("window of a %s leaf = %s, more than half its life", ttl, w)
		}
	}
	if peerNearExpiry(leaf(120*time.Second), at.Add(48*time.Second)) || !peerNearExpiry(leaf(120*time.Second), at.Add(95*time.Second)) {
		t.Error("a 120 s leaf must keep its connection at 48 s and close it at 95 s")
	}
}

// No refusal, log line or audit row carries key material or any certificate field but the peer's SPIFFE ID.
func TestInternalMTLS_LogsAndAuditCarryNoKeyMaterial(t *testing.T) {
	logs := captureSlog(t)
	f := newMTLSFixture(t, &touchStore{}, true, nil)
	otherRun := "spiffe://" + mtlsTrustDomain + "/agent-run/" + uuid.NewString()
	controlPlane := "spiffe://" + mtlsTrustDomain + identity.ControlPlanePath
	leaves := []tls.Certificate{
		mtlsLeaf(t, nil, f.id, f.now.Add(-time.Minute), f.now.Add(time.Hour)),
		f.leaf(t, controlPlane),
		f.leaf(t, otherRun),
		mtlsLeaf(t, f.ca, f.id, f.now.Add(-time.Hour), f.now.Add(10*time.Second)),
	}
	var bodies []string
	w := mtlsDecision(t, f, f.tok, nil, nil)
	bodies = append(bodies, w.Body.String())
	for _, l := range leaves {
		w := mtlsDecision(t, f, f.tok, mtlsState(l), nil)
		bodies = append(bodies, w.Body.String())
	}
	ts, pool := mtlsTLSServer(t, f.srv, false)
	if r := mtlsPost(t, mtlsClient(pool, &leaves[3], false), ts.URL, f.tok); r.code != http.StatusAccepted || !r.connClose {
		t.Fatalf("a leaf 10 s from expiry over a real handshake: %+v, want 202 with Connection: close", r)
	}
	if !strings.Contains(logs.String(), "peer_spiffe_id="+otherRun) || !strings.Contains(logs.String(), "peer_spiffe_id="+controlPlane) {
		t.Errorf("the refusals of a verified peer do not name its SPIFFE ID: %s", logs.String())
	}
	var trail []string
	for _, ev := range f.h.audit.snapshot() {
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		trail = append(trail, string(b))
	}
	for _, l := range leaves {
		key := l.PrivateKey.(*ecdsa.PrivateKey)
		dBytes := key.D.Bytes()
		forbidden := []string{
			"BEGIN", "PRIVATE KEY", mtlsSubjectMarker,
			base64.StdEncoding.EncodeToString(l.Leaf.Raw[:30]), base64.RawURLEncoding.EncodeToString(l.Leaf.Raw[:30]),
			hex.EncodeToString(l.Leaf.Raw[:32]), l.Leaf.SerialNumber.String(), hex.EncodeToString(l.Leaf.SerialNumber.Bytes()),
			hex.EncodeToString(dBytes), base64.StdEncoding.EncodeToString(dBytes[:30]),
		}
		for name, text := range map[string]string{"logs": logs.String(), "audit": strings.Join(trail, "\n"), "bodies": strings.Join(bodies, "\n")} {
			for _, s := range forbidden {
				if strings.Contains(text, s) {
					t.Errorf("%s carry %q, part of a certificate or its key", name, s)
				}
			}
		}
	}
}

// The accepted limit: a remote-hosted runner's proxy dials the internal listener directly with no run
// certificate, so with the switch on its first control-plane call is refused; with it off it is served.
func TestInternalMTLS_RemoteUnrelayedRefused(t *testing.T) {
	runner := uuid.New()
	remote := func() store.Store {
		return mtlsRunStore{touchStore: &touchStore{}, run: types.AgentRun{State: types.RunRunning, Placement: types.PlacementRemote, RunnerID: &runner, UpdatedAt: time.Now().UTC()}}
	}
	on := newMTLSFixture(t, remote(), true, nil)
	wantMTLSRefusal(t, on, mtlsDecision(t, on, on.tok, nil, nil), http.StatusUnauthorized, reasonInternalPeerRequired)
	off := newMTLSFixture(t, remote(), false, nil)
	wantServed(t, mtlsDecision(t, off, off.tok, nil, nil))
}

// A run's proxy is handed its SPIFFE ID as the client certificate to present only with the switch on and
// only when this daemon's own Kubernetes runner creates its sandbox: off, on another runner target, or on a
// runner (a Kubernetes one included, whose proxies hold no run certificate in this release) it gets none.
// The dispatch literal takes the value from proxyPeerID, so this is what a run's proxy is sent.
func TestProxyPeerID_OnlyOwnKubernetesProxies(t *testing.T) {
	kubeRunner := uuid.New()
	run := types.AgentRun{ID: uuid.New()}
	run.SPIFFEID = "spiffe://wardyn.local/agent-run/" + run.ID.String()
	onRunner := run
	onRunner.RunnerID = &kubeRunner
	on := &InternalMTLS{TrustDomain: "wardyn.local"}
	for _, tc := range []struct {
		name   string
		mtls   *InternalMTLS
		target string
		run    types.AgentRun
		want   string
	}{
		{"off, own Kubernetes runner", nil, "k8s", run, ""},
		{"on, own Kubernetes runner", on, "k8s", run, run.SPIFFEID},
		{"on, own docker runner", on, "docker", run, ""},
		{"on, no runner target", on, "", run, ""},
		{"on, a Kubernetes runner", on, "k8s", onRunner, ""},
	} {
		srv := &Server{cfg: Config{InternalMTLS: tc.mtls, RunnerTarget: tc.target}}
		seedRunnerCaps(t, srv, kubeRunner, runnerwire.Caps{EndsUnkept: true})
		if got := srv.proxyPeerID(tc.run); got != tc.want {
			t.Errorf("%s: proxyPeerID = %q, want %q", tc.name, got, tc.want)
		}
	}
	src, err := os.ReadFile("runs_dispatch.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(src), "ControlPlanePeerID: s.proxyPeerID(run),"); n != 1 {
		t.Fatalf("runs_dispatch.go sets the proxy's ControlPlanePeerID from proxyPeerID %d times, want 1", n)
	}
}

// A revive follows the current switch, not the one at first start: with it on, this daemon's own Kubernetes
// proxy is handed its run's SPIFFE ID whatever the stored config held; with it off, or on another runner
// target, a stored peer ID is removed, so the revived proxy presents no run certificate.
func TestReviveConfig_StampsPeerIDFromCurrentSwitch(t *testing.T) {
	ca := newMTLSCA(t, "wardyn.local")
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw}))
	run := types.AgentRun{ID: uuid.New()}
	run.SPIFFEID = "spiffe://wardyn.local/agent-run/" + run.ID.String()
	stale := "spiffe://wardyn.local/agent-run/" + uuid.New().String()
	on := &InternalMTLS{TrustDomain: "wardyn.local"}
	for _, tc := range []struct {
		name   string
		mtls   *InternalMTLS
		target string
		stored map[string]any // extra keys in the stored config
		want   string
	}{
		{"on, k8s, none stored", on, "k8s", nil, run.SPIFFEID},
		{"on, k8s, another stored", on, "k8s", map[string]any{"control_plane_peer_id": stale}, run.SPIFFEID},
		{"off, k8s, one stored", nil, "k8s", map[string]any{"control_plane_peer_id": run.SPIFFEID}, ""},
		{"on, docker, one stored", on, "docker", map[string]any{"control_plane_peer_id": run.SPIFFEID}, ""},
	} {
		srv := &Server{cfg: Config{InternalMTLS: tc.mtls, RunnerTarget: tc.target,
			ControlPlaneURL: "https://wardynd:8443", ControlPlaneCAPEM: caPEM}}
		m := map[string]any{"run_id": run.ID.String(), "control_plane_url": "http://wardynd:8080", "run_token": "tok"}
		for k, v := range tc.stored {
			m[k] = v
		}
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := srv.loadRenderedProxyConfig(raw, run)
		if err != nil {
			t.Fatalf("%s: loadRenderedProxyConfig: %v", tc.name, err)
		}
		if cfg.ControlPlanePeerID != tc.want || cfg.ControlPlaneURL != "https://wardynd:8443" {
			t.Errorf("%s: peer ID %q, URL %q; want %q and the current URL", tc.name, cfg.ControlPlanePeerID, cfg.ControlPlaneURL, tc.want)
		}
	}
}
