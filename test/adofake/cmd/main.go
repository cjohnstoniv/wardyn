// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command adofake serves test/entrafake AND test/adofake from ONE process, behind
// a TLS-terminating forward proxy, so the kind walk's Azure DevOps profile
// (scripts/lib/kind-sso-walk-ado.sh) can reach "login.microsoftonline.com" and
// "dev.azure.com" by their real names on a cluster with no Microsoft tenant.
//
// It is a TEST BINARY, not a product one: it is built only by
// deploy/kind/sso/overlay.sh into wardyn/test-adofake:local, never by `make
// agent-images`, never published by release.yml, and it belongs only on a
// throwaway cluster (deploy/kind/sso/adofake.yaml).
//
// THE SHAPE, and why each part exists:
//
//   - ONE listener that is a forward proxy. wardynd reaches it as its
//     WARDYN_DAEMON_PROXY_URL, a run's proxy sidecar as site-config's
//     upstream_proxy_url, and the walk's curl through a port-forward. A CONNECT
//     to one of the two names is answered by terminating TLS with a leaf the
//     walk's own CA signs (the CA every Wardyn component trusts through
//     WARDYN_TRUSTED_CA_FILE); any other name is refused. That is the corporate
//     TLS-inspecting middlebox Wardyn already supports, so no product code
//     learns a test-only way to re-point either Microsoft name.
//   - BOTH FAKES IN ONE PROCESS, so the question "which tokens does Azure
//     DevOps trust" has an answer reachable only in-process: every access token
//     entrafake mints is registered with adofake at mint time (entrafake.OnIssue)
//     together with the SUBJECT it was minted for. Nothing on the network can
//     register a token.
//   - /_seen, a read-only count of what reached Azure DevOps, per caller
//     SUBJECT. It is the walk's one observation that is not Wardyn asserting
//     about itself.
package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/test/adofake"
	"github.com/cjohnstoniv/wardyn/test/entrafake"
)

// entraHost is the one name the Entra fake answers as.
const entraHost = "login.microsoftonline.com"

type config struct {
	caCertPEM, caKeyPEM    []byte
	clientID, clientSecret string
	redirectURI            string
	identities             []entrafake.Identity
	org, project, repo     string
	repoDir                string
}

// fake is the running process: the two fakes, the front, and the caller book.
type fake struct {
	ca     *x509.Certificate
	caKey  any
	entra  http.Handler
	tenant string
	ado    *adofake.Server
	adoH   http.Handler
	leaves sync.Map // host -> *tls.Certificate

	mu        sync.Mutex
	subjectOf map[string]string // access token -> subject
	issued    map[string]int    // subject -> access tokens minted
}

func main() {
	addr := flag.String("addr", ":3128", "listen address of the forward proxy (and /_seen, /healthz)")
	caCert := flag.String("ca-cert", "/etc/adofake/ca.crt", "PEM CA certificate the leaves are signed with")
	caKey := flag.String("ca-key", "/etc/adofake/ca.key", "PEM private key of -ca-cert")
	clientID := flag.String("client-id", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "the console's app registration client id")
	clientSecret := flag.String("client-secret", "", "the app registration's secret (empty = a public client)")
	redirectURI := flag.String("redirect-uri", "", "the console's OIDC callback URL, the one redirect /authorize accepts")
	identities := flag.String("identities", "", "the sign-in picker: comma-separated username=subject pairs")
	org := flag.String("org", "contoso", "Azure DevOps organisation")
	project := flag.String("project", "proj", "project in -org")
	repo := flag.String("repo", "app", "git repository in -project")
	flag.Parse()

	cfg := config{clientID: *clientID, clientSecret: *clientSecret, redirectURI: *redirectURI,
		org: *org, project: *project, repo: *repo}
	var err error
	if cfg.caCertPEM, err = os.ReadFile(*caCert); err != nil {
		log.Fatalf("adofake: %v", err)
	}
	if cfg.caKeyPEM, err = os.ReadFile(*caKey); err != nil {
		log.Fatalf("adofake: %v", err)
	}
	if cfg.identities, err = parseIdentities(*identities); err != nil {
		log.Fatalf("adofake: %v", err)
	}
	if cfg.repoDir, err = os.MkdirTemp("", "adofake-repos"); err != nil {
		log.Fatalf("adofake: %v", err)
	}
	f, err := newFake(cfg)
	if err != nil {
		log.Fatalf("adofake: %v", err)
	}
	log.Printf("adofake: forward proxy for %s (tenant %s) and dev.azure.com (organisation %s) on %s, %d identities",
		entraHost, f.tenant, cfg.org, *addr, len(cfg.identities))
	srv := &http.Server{Addr: *addr, Handler: f, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// parseIdentities reads "user=subject,user=subject".
func parseIdentities(raw string) ([]entrafake.Identity, error) {
	var out []entrafake.Identity
	for _, pair := range strings.Split(raw, ",") {
		if strings.TrimSpace(pair) == "" {
			continue
		}
		user, sub, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || user == "" || sub == "" {
			return nil, fmt.Errorf("-identities: %q is not username=subject", pair)
		}
		out = append(out, entrafake.Identity{Username: user, Subject: sub})
	}
	return out, nil
}

func newFake(cfg config) (*fake, error) {
	ca, key, err := parseCA(cfg.caCertPEM, cfg.caKeyPEM)
	if err != nil {
		return nil, err
	}
	f := &fake{ca: ca, caKey: key, subjectOf: map[string]string{}, issued: map[string]int{}}

	es, eh := entrafake.NewHandler()
	es.SetBaseURL("https://" + entraHost)
	es.SetClientID(cfg.clientID)
	es.SetClientSecret(cfg.clientSecret)
	es.SetRedirectURI(cfg.redirectURI)
	es.SetIdentities(cfg.identities...)
	consented, err := adoscope.ScopesFor([]adoscope.Capability{adoscope.CapRead, adoscope.CapCodeWrite, adoscope.CapWorkWrite})
	if err != nil {
		return nil, err
	}
	es.SetConsentedScopes(consented...)
	f.entra = eh
	f.tenant = es.TenantID()

	f.ado, f.adoH = adofake.NewHandler()
	f.ado.AddProject(cfg.org, "", cfg.project)
	bare, err := fixtureRepo(cfg.repoDir, cfg.repo)
	if err != nil {
		return nil, err
	}
	f.ado.RegisterRepo(cfg.org, cfg.project, cfg.repo, bare)

	// THE REGISTRATION PATH: in-process only. A token is trusted by Azure
	// DevOps exactly when the Entra fake minted it, with the scopes it minted it
	// with (unqualified, the way the resource checks them).
	es.OnIssue(func(it entrafake.IssuedToken) {
		scopes := make([]string, 0, len(it.Scopes))
		for _, sc := range it.Scopes {
			scopes = append(scopes, strings.TrimPrefix(sc, adoscope.ResourceID+"/"))
		}
		f.ado.RegisterToken(it.AccessToken, scopes...)
		f.mu.Lock()
		f.subjectOf[it.AccessToken] = it.Subject
		f.issued[it.Subject]++
		f.mu.Unlock()
	})
	return f, nil
}

func parseCA(certPEM, keyPEM []byte) (*x509.Certificate, any, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, nil, errors.New("the CA certificate or key is not PEM")
	}
	ca, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("CA certificate: %w", err)
	}
	key, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		if ek, eerr := x509.ParseECPrivateKey(kb.Bytes); eerr == nil {
			return ca, ek, nil
		}
		return nil, nil, fmt.Errorf("CA key: %w", err)
	}
	return ca, key, nil
}

// fixtureRepo makes a bare repository with one commit on main.
func fixtureRepo(dir, name string) (string, error) {
	work, bare := filepath.Join(dir, "work"), filepath.Join(dir, name+".git")
	run := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=adofake", "GIT_AUTHOR_EMAIL=adofake@example.invalid",
			"GIT_COMMITTER_NAME=adofake", "GIT_COMMITTER_EMAIL=adofake@example.invalid")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %w: %s", args, err, out)
		}
		return nil
	}
	for _, args := range [][]string{
		{"-c", "init.defaultBranch=main", "init", "-q", work},
		{"-C", work, "commit", "-q", "--allow-empty", "-m", "fixture"},
		{"clone", "-q", "--bare", work, bare},
	} {
		if err := run(args...); err != nil {
			return "", err
		}
	}
	return bare, nil
}

// route answers which fake serves host, or nil for a name this front refuses.
func (f *fake) route(host string) http.Handler {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	switch {
	case host == entraHost:
		return f.entra
	case host == "dev.azure.com", strings.HasSuffix(host, ".dev.azure.com"), strings.HasSuffix(host, ".visualstudio.com"):
		return f.adoH
	}
	return nil
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		f.serveConnect(w, r)
		return
	}
	switch r.URL.Path {
	case "/healthz":
		_, _ = io.WriteString(w, "ok\n")
	case "/_seen":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.seen())
	default:
		http.Error(w, "adofake: only CONNECT to "+entraHost+" or dev.azure.com is served", http.StatusForbidden)
	}
}

// seenCaller is one subject's traffic as Azure DevOps saw it.
type seenCaller struct {
	Requests   int            `json:"requests"`
	Authorized int            `json:"authorized"`
	Endpoints  map[string]int `json:"endpoints"`
}

// seen tallies adofake's request log by the SUBJECT each presented token was
// minted for. A request with no token, or one entrafake never minted, is
// counted under "(none)" / "(unknown)" — never guessed onto a person.
func (f *fake) seen() map[string]any {
	f.mu.Lock()
	subjectOf := make(map[string]string, len(f.subjectOf))
	for k, v := range f.subjectOf {
		subjectOf[k] = v
	}
	issued := make(map[string]int, len(f.issued))
	for k, v := range f.issued {
		issued[k] = v
	}
	f.mu.Unlock()
	callers := map[string]*seenCaller{}
	for _, req := range f.ado.Requests() {
		who := "(none)"
		if req.Token != "" {
			who = "(unknown)"
			if sub, ok := subjectOf[req.Token]; ok {
				who = sub
			}
		}
		c := callers[who]
		if c == nil {
			c = &seenCaller{Endpoints: map[string]int{}}
			callers[who] = c
		}
		c.Requests++
		if req.Authorized {
			c.Authorized++
		}
		c.Endpoints[string(req.Endpoint)]++
	}
	return map[string]any{"callers": callers, "tokens_issued": issued}
}

func (f *fake) serveConnect(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	h := f.route(host)
	if h == nil {
		http.Error(w, "adofake: "+host+" is not a name this fake answers for", http.StatusForbidden)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "adofake: cannot hijack", http.StatusInternalServerError)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		_ = conn.Close()
		return
	}
	tc := tls.Server(&bufferedConn{Conn: conn, r: brw.Reader}, &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := hello.ServerName
			if name == "" {
				name = host
			}
			return f.leaf(name)
		},
	})
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	_ = srv.Serve(&oneConnListener{c: tc})
}

// leaf mints (once per name) a serving certificate for host, signed by the CA.
func (f *fake) leaf(host string) (*tls.Certificate, error) {
	if c, ok := f.leaves.Load(host); ok {
		return c.(*tls.Certificate), nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return nil, err
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(7 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, f.ca, &key.PublicKey, f.caKey)
	if err != nil {
		return nil, err
	}
	c := &tls.Certificate{Certificate: [][]byte{der, f.ca.Raw}, PrivateKey: key}
	f.leaves.Store(host, c)
	return c, nil
}

// bufferedConn replays bytes the CONNECT reader already buffered.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// oneConnListener serves exactly one connection, then reports closed.
type oneConnListener struct {
	c    net.Conn
	once sync.Once
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = l.c })
	if c != nil {
		return c, nil
	}
	return nil, net.ErrClosed
}
func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return l.c.LocalAddr() }
