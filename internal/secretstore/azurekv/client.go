// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package azurekv

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// Auth modes (WARDYN_AZURE_AUTH).
const (
	// AuthWorkloadIdentity exchanges a projected Kubernetes token for an Entra
	// access token (a federated credential on the app registration or the
	// user-assigned identity).
	AuthWorkloadIdentity = "workload-identity"
	// AuthManagedIdentity asks the VM's instance metadata service.
	AuthManagedIdentity = "managed-identity"
)

const (
	// apiVersion is the Key Vault data-plane API version (design §2.3a.3).
	apiVersion = "2025-07-01"
	// scope and resource name Key Vault to Entra (v2 endpoint, and IMDS).
	scope    = "https://vault.azure.net/.default"
	resource = "https://vault.azure.net"
	// imdsURL is the managed-identity endpoint every Azure VM serves. It is
	// link-local plain HTTP by design and is never sent through a proxy.
	imdsURL = "http://169.254.169.254/metadata/identity/oauth2/token"
)

// retries is how many times a 429, a 5xx or a network failure is retried
// before the call counts as transient (design §2.3a.2, shared with vaultkv).
const retries = 3

// refreshEvery bounds the token refreshes a 401 triggers.
const refreshEvery = 30 * time.Second

// client is a minimal Key Vault Secrets client: stdlib only, its own TLS
// config, one Entra access token it refreshes before expiry. It never logs or
// returns a request body, so a value it writes can never reach an error or a
// log line.
type client struct {
	base    *url.URL // https://<vault>.vault.azure.net
	host    string   // lowercase; the <vault-host> of every ref
	http    *http.Client
	timeout time.Duration
	backoff time.Duration

	auth               string
	authority          *url.URL
	tenant, clientID   string
	federatedTokenFile string
	imds               string
	imdsHTTP           *http.Client

	mu        sync.Mutex
	token     string
	refreshAt time.Time

	refreshMu   sync.Mutex // serialises token fetches; guards lastFetched
	lastFetched time.Time
}

// kvError is a non-2xx answer (other than the 404 callers interpret). status
// 0 is a transport failure. msg is Key Vault's own message, which names the
// secret at most, never its value.
type kvError struct {
	op        string
	status    int
	code, msg string
	cause     error
}

func (e *kvError) Error() string {
	switch {
	case e.status == 0:
		return fmt.Sprintf("key vault %s: %v", e.op, e.cause)
	case e.code != "":
		return fmt.Sprintf("key vault %s: %d %s: %s", e.op, e.status, e.code, e.msg)
	default:
		return fmt.Sprintf("key vault %s: %d", e.op, e.status)
	}
}

// Unwrap makes a transient failure match secretstore.ErrUnavailable. 401 and
// 403 are definitive on purpose: revoking Wardyn's role at the vault must
// bite at once (design rule 21).
func (e *kvError) Unwrap() error {
	if transient(e.status) {
		return secretstore.ErrUnavailable
	}
	return e.cause
}

func transient(status int) bool {
	return status == 0 || status == http.StatusTooManyRequests || status >= 500
}

func statusOf(err error) int {
	var ke *kvError
	if errors.As(err, &ke) {
		return ke.status
	}
	return -1
}

// tokenError is a failure to get an Entra token. At runtime it is transient
// (design §2.3a.3): the identity is still configured, the token endpoint just
// did not answer usefully. At boot it fails the start.
type tokenError struct{ err error }

func (e *tokenError) Error() string { return "entra token: " + e.err.Error() }
func (e *tokenError) Unwrap() []error {
	return []error{secretstore.ErrUnavailable, e.err}
}

// httpsURL parses setting's value and refuses plain http:// to anything but
// a loopback host (the CS-7 rule): the vault answers with the values, and the
// token endpoint with a bearer token for all of them.
func httpsURL(setting, raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%s %q is not an http(s) URL", setting, raw)
	}
	if u.Scheme == "http" && !loopback(u.Hostname()) {
		return nil, fmt.Errorf("%s %q is plain http:// to a non-loopback host; use https://", setting, raw)
	}
	return u, nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func newClient(cfg Config) (*client, error) {
	base, err := httpsURL("WARDYN_AZURE_KV_URL", cfg.VaultURL)
	if err != nil {
		return nil, err
	}
	if base.Path != "" {
		return nil, fmt.Errorf("WARDYN_AZURE_KV_URL %q has a path; give the vault's base URL (https://<vault>.vault.azure.net)", cfg.VaultURL)
	}
	c := &client{
		base: base, host: strings.ToLower(base.Host), timeout: cfg.Timeout, backoff: 200 * time.Millisecond,
		auth: cfg.Auth, tenant: cfg.TenantID, clientID: cfg.ClientID, federatedTokenFile: cfg.FederatedTokenFile,
		imds: imdsURL,
	}
	if c.timeout <= 0 {
		c.timeout = 5 * time.Second
	}
	switch cfg.Auth {
	case AuthWorkloadIdentity:
		if cfg.TenantID == "" || cfg.ClientID == "" {
			return nil, fmt.Errorf("WARDYN_AZURE_TENANT_ID and WARDYN_AZURE_CLIENT_ID are required with WARDYN_AZURE_AUTH=%s", AuthWorkloadIdentity)
		}
		if strings.ContainsAny(cfg.TenantID, "/?#") {
			return nil, fmt.Errorf("WARDYN_AZURE_TENANT_ID %q is not a tenant id", cfg.TenantID)
		}
		if cfg.FederatedTokenFile == "" {
			return nil, fmt.Errorf("no federated token file: set WARDYN_AZURE_FEDERATED_TOKEN_FILE, or run where the workload identity webhook sets AZURE_FEDERATED_TOKEN_FILE")
		}
		host := cfg.AuthorityHost
		if host == "" {
			host = "https://login.microsoftonline.com"
		}
		if c.authority, err = httpsURL("WARDYN_AZURE_AUTHORITY_HOST", host); err != nil {
			return nil, err
		}
	case AuthManagedIdentity:
		// Link-local and never proxied: a proxy would see the token.
		c.imdsHTTP = &http.Client{Transport: &http.Transport{Proxy: nil}}
	default:
		return nil, fmt.Errorf("WARDYN_AZURE_AUTH %q is not %q or %q", cfg.Auth, AuthWorkloadIdentity, AuthManagedIdentity)
	}
	tlsCfg, err := tlsConfig(cfg.CACertFile)
	if err != nil {
		return nil, err
	}
	// A clone of the boot transport keeps WARDYN_DAEMON_PROXY_URL and its
	// NO_PROXY; the TLS config is always this client's own.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	c.http = &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return c, nil
}

// tlsConfig adds caFile (WARDYN_TRUSTED_CA_FILE) to the system roots.
func tlsConfig(caFile string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile == "" {
		return cfg, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read the trusted CA bundle: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("the trusted CA bundle %s holds no PEM certificate", caFile)
	}
	cfg.RootCAs = pool
	return cfg, nil
}

// accessToken returns the cached token, fetching a new one when less than
// five minutes (or half its life, if shorter) remain.
func (c *client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	tok, at := c.token, c.refreshAt
	c.mu.Unlock()
	if tok != "" && time.Now().Before(at) {
		return tok, nil
	}
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	c.mu.Lock()
	tok, at = c.token, c.refreshAt
	c.mu.Unlock()
	if tok != "" && time.Now().Before(at) {
		return tok, nil // another caller fetched it meanwhile
	}
	return c.fetch(ctx)
}

// forceRefresh answers a 401 met with token used: it reports whether a retry
// can use a different token. A new one is fetched at most once per
// refreshEvery, since an identity the vault no longer accepts would otherwise
// cost a token exchange per call.
func (c *client) forceRefresh(ctx context.Context, used string) bool {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	c.mu.Lock()
	cur := c.token
	c.mu.Unlock()
	if cur != used {
		return true // fetched by another caller since
	}
	if time.Since(c.lastFetched) < refreshEvery {
		return false
	}
	_, err := c.fetch(ctx)
	return err == nil
}

// fetch gets a new token. Callers hold refreshMu.
func (c *client) fetch(ctx context.Context) (string, error) {
	var (
		tok string
		ttl time.Duration
		err error
	)
	if c.auth == AuthManagedIdentity {
		tok, ttl, err = c.fromIMDS(ctx)
	} else {
		tok, ttl, err = c.fromFederatedToken(ctx)
	}
	if err != nil {
		return "", &tokenError{err}
	}
	margin := 5 * time.Minute
	if ttl/2 < margin {
		margin = ttl / 2
	}
	c.mu.Lock()
	c.token, c.refreshAt = tok, time.Now().Add(ttl-margin)
	c.mu.Unlock()
	c.lastFetched = time.Now()
	return tok, nil
}

type tokenResp struct {
	AccessToken string          `json:"access_token"`
	ExpiresIn   json.RawMessage `json:"expires_in"` // a number from Entra, a string from IMDS
	Error       string          `json:"error"`
	Description string          `json:"error_description"`
}

func (r tokenResp) parse(from string) (string, time.Duration, error) {
	secs, err := strconv.Atoi(strings.Trim(string(r.ExpiresIn), `"`))
	if r.AccessToken == "" || err != nil || secs <= 0 {
		return "", 0, fmt.Errorf("%s returned no usable access token", from)
	}
	return r.AccessToken, time.Duration(secs) * time.Second, nil
}

// fromFederatedToken is the documented client-credentials exchange with a
// federated credential. The projected token is re-read at every exchange:
// the kubelet refreshes it in place.
func (c *client) fromFederatedToken(ctx context.Context) (string, time.Duration, error) {
	b, err := os.ReadFile(c.federatedTokenFile)
	if err != nil {
		return "", 0, fmt.Errorf("read the federated token file: %w", err)
	}
	assertion := strings.TrimSpace(string(b))
	if assertion == "" {
		return "", 0, fmt.Errorf("the federated token file %s is empty", c.federatedTokenFile)
	}
	form := url.Values{
		"client_id":             {c.clientID},
		"scope":                 {scope},
		"grant_type":            {"client_credentials"},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {assertion},
	}
	u := c.authority.JoinPath(c.tenant, "oauth2", "v2.0", "token").String()
	var r tokenResp
	if err := c.tokenCall(ctx, c.http, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		return req, err
	}, &r); err != nil {
		return "", 0, err
	}
	return r.parse("the Entra token endpoint")
}

// fromIMDS asks the VM's managed identity for a Key Vault token. ClientID,
// when set, selects a user-assigned identity.
func (c *client) fromIMDS(ctx context.Context) (string, time.Duration, error) {
	q := url.Values{"api-version": {"2018-02-01"}, "resource": {resource}}
	if c.clientID != "" {
		q.Set("client_id", c.clientID)
	}
	u := c.imds + "?" + q.Encode()
	var r tokenResp
	if err := c.tokenCall(ctx, c.imdsHTTP, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err == nil {
			req.Header.Set("Metadata", "true")
		}
		return req, err
	}, &r); err != nil {
		return "", 0, err
	}
	return r.parse("the instance metadata service")
}

// tokenCall sends a token request with the same retries as a vault call. An
// error names the endpoint's error code and description, never the request.
func (c *client) tokenCall(ctx context.Context, hc *http.Client, build func(context.Context) (*http.Request, error), out *tokenResp) error {
	status, raw, _, err := c.send(ctx, hc, "token", build)
	if err != nil {
		return err
	}
	_ = json.Unmarshal(raw, out)
	if status/100 != 2 {
		if out.Error != "" {
			return fmt.Errorf("token endpoint %d: %s: %s", status, out.Error, firstLine(out.Description))
		}
		return fmt.Errorf("token endpoint %d", status)
	}
	return nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

// send makes one request, retrying a transient answer (429, 5xx, network)
// with backoff, honouring Retry-After up to 10 s. It returns the last answer:
// a transport failure is the only error.
func (c *client) send(ctx context.Context, hc *http.Client, op string, build func(context.Context) (*http.Request, error)) (int, []byte, http.Header, error) {
	for attempt := 0; ; attempt++ {
		status, raw, hdr, err := c.sendOnce(ctx, hc, build)
		if (err == nil && !transient(status)) || attempt == retries {
			if err != nil {
				return 0, nil, nil, &kvError{op: op, cause: err}
			}
			return status, raw, hdr, nil
		}
		wait := c.backoff << attempt
		if s, perr := strconv.Atoi(hdr.Get("Retry-After")); perr == nil && s > 0 {
			wait = max(wait, min(time.Duration(s)*time.Second, 10*time.Second))
		}
		select {
		case <-ctx.Done():
			if err == nil {
				return status, raw, hdr, nil
			}
			return 0, nil, nil, &kvError{op: op, cause: err}
		case <-time.After(wait):
		}
	}
}

func (c *client) sendOnce(ctx context.Context, hc *http.Client, build func(context.Context) (*http.Request, error)) (int, []byte, http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := build(ctx)
	if err != nil {
		return 0, nil, http.Header{}, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, http.Header{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, resp.Header, err
	}
	return resp.StatusCode, raw, resp.Header, nil
}

// call makes one Key Vault call on path (relative to the vault, no leading
// "/", or an absolute nextLink on this vault). The status is returned with a
// nil error for 2xx and 404, which callers interpret; every other answer is a
// *kvError. A 401 fetches a new token (bounded) and retries once.
func (c *client) call(ctx context.Context, method, path string, in, out any) (int, error) {
	u, err := c.url(path)
	if err != nil {
		return 0, err
	}
	var body []byte
	if in != nil {
		if body, err = json.Marshal(in); err != nil {
			return 0, fmt.Errorf("key vault %s: encode: %w", method, err)
		}
	}
	op := method + " " + u.Path
	status, raw, used, err := c.authed(ctx, method, u.String(), op, body)
	if err == nil && status == http.StatusUnauthorized && c.forceRefresh(ctx, used) {
		status, raw, _, err = c.authed(ctx, method, u.String(), op, body)
	}
	if err != nil {
		return 0, err
	}
	switch {
	case status == http.StatusNotFound:
		return status, nil
	case status/100 != 2:
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return status, &kvError{op: op, status: status, code: e.Error.Code, msg: firstLine(e.Error.Message)}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return status, fmt.Errorf("key vault %s: decode: %w", op, err)
		}
	}
	return status, nil
}

func (c *client) authed(ctx context.Context, method, u, op string, body []byte) (int, []byte, string, error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return 0, nil, "", err
	}
	status, raw, _, err := c.send(ctx, c.http, op, func(ctx context.Context) (*http.Request, error) {
		var r io.Reader
		if body != nil {
			r = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, r)
		if err != nil {
			return nil, err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		return req, nil
	})
	return status, raw, tok, err
}

// url resolves path against the vault, adding the API version. An absolute
// URL (a list's nextLink) must name this vault exactly: the bearer token is
// never sent anywhere else.
func (c *client) url(path string) (*url.URL, error) {
	var u *url.URL
	if strings.HasPrefix(path, "https://") || strings.HasPrefix(path, "http://") {
		p, err := url.Parse(path)
		if err != nil || p.Scheme != c.base.Scheme || !strings.EqualFold(p.Host, c.base.Host) || p.User != nil {
			return nil, fmt.Errorf("key vault: refusing to follow a link off this vault (%s)", c.base.Host)
		}
		u = p
	} else {
		p, err := url.Parse(path)
		if err != nil {
			return nil, fmt.Errorf("key vault: bad path %q: %w", path, err)
		}
		u = c.base.ResolveReference(&url.URL{Path: "/" + p.Path, RawQuery: p.RawQuery})
	}
	q := u.Query()
	if q.Get("api-version") == "" {
		q.Set("api-version", apiVersion)
		u.RawQuery = q.Encode()
	}
	return u, nil
}
