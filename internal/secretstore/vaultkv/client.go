// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vaultkv

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// Auth modes (WARDYN_VAULT_AUTH).
const (
	AuthKubernetes = "kubernetes"
	AuthTokenFile  = "token-file"
)

// retries is how many times a 429, a 5xx or a network failure is retried
// before the call counts as transient (design §2.3a.2).
const retries = 3

// reloginEvery bounds the logins a 401/403 triggers. A revoked policy answers
// 403 to every call, and each login is a Kubernetes TokenReview and a line in
// both audit logs.
const reloginEvery = 30 * time.Second

// client is a minimal Vault HTTP API client: stdlib only, its own TLS config,
// one token it keeps alive. It never logs or returns a request body, so a
// value it writes can never reach an error or a log line.
type client struct {
	base    *url.URL
	ns      string
	http    *http.Client
	timeout time.Duration
	backoff time.Duration

	authMode, authMount, role, jwtFile, tokenFile string

	mu        sync.Mutex
	token     string
	ttl       time.Duration
	renewable bool

	loginMu   sync.Mutex // serialises relogin; guards reloginAt
	reloginAt time.Time
}

// vaultError is a non-2xx answer. status 0 is a transport failure.
type vaultError struct {
	method, path string
	status       int
	msgs         []string
	cause        error
}

func (e *vaultError) Error() string {
	where := e.method + " " + e.path
	switch {
	case e.status == 0:
		return fmt.Sprintf("vault %s: %v", where, e.cause)
	case len(e.msgs) > 0:
		return fmt.Sprintf("vault %s: %d: %s", where, e.status, strings.Join(e.msgs, "; "))
	default:
		return fmt.Sprintf("vault %s: %d", where, e.status)
	}
}

// Unwrap makes a transient failure match secretstore.ErrUnavailable. 401 and
// 403 are definitive on purpose: revoking Wardyn's access at Vault must bite
// at once, not ride out a grace period (design rule 21).
func (e *vaultError) Unwrap() error {
	if transient(e.status) {
		return secretstore.ErrUnavailable
	}
	return nil
}

func transient(status int) bool {
	return status == 0 || status == http.StatusTooManyRequests || status >= 500
}

func statusOf(err error) int {
	var ve *vaultError
	if errors.As(err, &ve) {
		return ve.status
	}
	return -1
}

// newClient validates the address and builds the transport. It does not
// contact Vault; login does.
func newClient(cfg Config) (*client, error) {
	u, err := url.Parse(strings.TrimRight(cfg.Addr, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("WARDYN_VAULT_ADDR %q is not an http(s) URL", cfg.Addr)
	}
	if u.Scheme == "http" && !loopback(u.Hostname()) {
		return nil, fmt.Errorf("WARDYN_VAULT_ADDR %q is plain http:// to a non-loopback host; use https:// (credentials cross this connection)", cfg.Addr)
	}
	tlsCfg, err := tlsConfig(cfg.CACertFile)
	if err != nil {
		return nil, err
	}
	// A clone of the boot transport keeps WARDYN_DAEMON_PROXY_URL and its
	// NO_PROXY; the TLS config is always this client's own, never shared with
	// another transport.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	c := &client{
		base: u, ns: cfg.Namespace, http: &http.Client{Transport: tr}, timeout: cfg.Timeout, backoff: 200 * time.Millisecond,
		authMode: cfg.Auth, authMount: cfg.AuthMount, role: cfg.Role, jwtFile: cfg.K8sTokenFile, tokenFile: cfg.TokenFile,
	}
	if c.timeout <= 0 {
		c.timeout = 5 * time.Second
	}
	return c, nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// tlsConfig adds caFile (WARDYN_VAULT_CACERT_FILE, else WARDYN_TRUSTED_CA_FILE)
// to the system roots.
func tlsConfig(caFile string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile == "" {
		return cfg, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read the Vault CA bundle: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("the Vault CA bundle %s holds no PEM certificate", caFile)
	}
	cfg.RootCAs = pool
	return cfg, nil
}

// call makes one API call with the current token. A 401/403 re-authenticates
// (a rotated token file, an expired login) and retries once; a transient
// failure is retried with backoff. The HTTP status is returned alongside a
// nil error only for 2xx, and for a 404 to a GET, which callers interpret as
// "nothing there". A 404 to a write or a DELETE is an error: Vault answers
// one when no engine is mounted at the path, and nothing was stored or removed.
func (c *client) call(ctx context.Context, method, path string, in, out any) (int, error) {
	status, err := c.retrying(ctx, method, path, in, out, true)
	if s := statusOf(err); s == http.StatusForbidden || s == http.StatusUnauthorized {
		if aerr := c.relogin(ctx); aerr != nil {
			return status, fmt.Errorf("%w (re-authenticating after it failed too: %v)", err, aerr)
		}
		status, err = c.retrying(ctx, method, path, in, out, true)
	}
	return status, err
}

// relogin logs in again at most once per reloginEvery. Inside the window the
// caller retries with the token the last login fetched; a call that met the
// 403 while that login was in flight waits for it.
func (c *client) relogin(ctx context.Context) error {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if time.Since(c.reloginAt) < reloginEvery {
		return nil
	}
	c.reloginAt = time.Now()
	return c.login(ctx)
}

func (c *client) retrying(ctx context.Context, method, path string, in, out any, authed bool) (int, error) {
	var err error
	status := 0
	for attempt := 0; ; attempt++ {
		status, err = c.once(ctx, method, path, in, out, authed)
		if err == nil || !transient(statusOf(err)) || attempt == retries {
			return status, err
		}
		select {
		case <-ctx.Done():
			return status, err
		case <-time.After(c.backoff << attempt):
		}
	}
}

func (c *client) once(ctx context.Context, method, path string, in, out any, authed bool) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, fmt.Errorf("vault %s %s: encode: %w", method, path, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+"/v1/"+path, body)
	if err != nil {
		return 0, fmt.Errorf("vault %s %s: %w", method, path, err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.ns != "" {
		req.Header.Set("X-Vault-Namespace", c.ns)
	}
	if authed {
		c.mu.Lock()
		req.Header.Set("X-Vault-Token", c.token)
		c.mu.Unlock()
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, &vaultError{method: method, path: path, cause: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, &vaultError{method: method, path: path, cause: err}
	}
	switch {
	case resp.StatusCode == http.StatusNotFound && method == http.MethodGet:
		return resp.StatusCode, nil
	case resp.StatusCode/100 != 2:
		var e struct {
			Errors []string `json:"errors"`
		}
		_ = json.Unmarshal(raw, &e)
		return resp.StatusCode, &vaultError{method: method, path: path, status: resp.StatusCode, msgs: e.Errors}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("vault %s %s: decode: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

type authResp struct {
	Auth struct {
		ClientToken   string `json:"client_token"`
		LeaseDuration int    `json:"lease_duration"`
		Renewable     bool   `json:"renewable"`
	} `json:"auth"`
}

// login obtains a fresh token. Kubernetes auth re-reads the projected service
// account token every time (the kubelet refreshes it in place); token-file
// re-reads the file (a Vault Agent sink rotates it) and looks the token up to
// learn its TTL. Nothing is cached from the file between logins.
func (c *client) login(ctx context.Context) error {
	switch c.authMode {
	case AuthKubernetes:
		jwt, err := readTrimmed(c.jwtFile, "WARDYN_VAULT_K8S_TOKEN_FILE")
		if err != nil {
			return err
		}
		var r authResp
		if _, err := c.retrying(ctx, http.MethodPost, "auth/"+c.authMount+"/login",
			map[string]string{"role": c.role, "jwt": jwt}, &r, false); err != nil {
			return fmt.Errorf("vault kubernetes login (role %q): %w", c.role, err)
		}
		if r.Auth.ClientToken == "" {
			return fmt.Errorf("vault kubernetes login (role %q) returned no token", c.role)
		}
		c.setToken(r.Auth.ClientToken, r.Auth.LeaseDuration, r.Auth.Renewable)
		return nil
	case AuthTokenFile:
		tok, err := readTrimmed(c.tokenFile, "WARDYN_VAULT_TOKEN_FILE")
		if err != nil {
			return err
		}
		c.setToken(tok, 0, false)
		var r struct {
			Data struct {
				TTL       int  `json:"ttl"`
				Renewable bool `json:"renewable"`
			} `json:"data"`
		}
		if _, err := c.retrying(ctx, http.MethodGet, "auth/token/lookup-self", nil, &r, true); err != nil {
			return fmt.Errorf("vault token from WARDYN_VAULT_TOKEN_FILE: %w", err)
		}
		c.setToken(tok, r.Data.TTL, r.Data.Renewable)
		return nil
	}
	return fmt.Errorf("WARDYN_VAULT_AUTH %q is not %q or %q", c.authMode, AuthKubernetes, AuthTokenFile)
}

func readTrimmed(path, setting string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%s is not set", setting)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", setting, err)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("%s (%s) is empty", setting, path)
	}
	return v, nil
}

func (c *client) setToken(tok string, ttlSeconds int, renewable bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token, c.ttl, c.renewable = tok, time.Duration(ttlSeconds)*time.Second, renewable
}

// renewAfter is when the next renewal is due: two thirds of the token's TTL,
// or 0 for a token that does not expire.
func (c *client) renewAfter() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ttl * 2 / 3
}

// renew extends the token (renew-self) or, when it cannot be renewed, logs in
// again.
func (c *client) renew(ctx context.Context) error {
	c.mu.Lock()
	renewable := c.renewable
	c.mu.Unlock()
	if renewable {
		var r authResp
		_, err := c.retrying(ctx, http.MethodPost, "auth/token/renew-self", map[string]any{}, &r, true)
		if err == nil && r.Auth.LeaseDuration > 0 {
			c.mu.Lock()
			c.ttl = time.Duration(r.Auth.LeaseDuration) * time.Second
			c.mu.Unlock()
			return nil
		}
	}
	return c.login(ctx)
}

// keepAlive renews the token at two thirds of its TTL until ctx ends. A
// failed renewal retries on a short timer; meanwhile a call that meets a 403
// logs in again by itself.
func (c *client) keepAlive(ctx context.Context) {
	for {
		wait := c.renewAfter()
		if wait <= 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		for {
			err := c.renew(ctx)
			if err == nil {
				break
			}
			slog.Warn("vaultkv: renewing the Vault token failed; retrying in 30s", slog.Any("err", err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
		}
	}
}
