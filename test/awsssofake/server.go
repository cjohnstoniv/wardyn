// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package awsssofake is a local, unsigned fake of the two AWS IAM Identity
// Center (SSO) HTTP services the AWS CLI/SDK talks to during `aws sso login`
// and later role-credential resolution:
//
//   - sso-oidc (RegisterClient, StartDeviceAuthorization, CreateToken)
//   - sso "portal" (GetRoleCredentials, ListAccounts, ListAccountRoles)
//
// All four operations Wardyn cares about are modeled `authtype: none` in
// botocore's service-2.json (rest-json, no SigV4) — confirmed by extracting
// the service model from the AWS CLI v2 image
// (deploy/images/aws-sso/Dockerfile) at
// /usr/local/aws-cli/v2/*/dist/awscli/botocore/data/{sso,sso-oidc}/*/service-2.json.
// That means a plain unsigned JSON server is sufficient to impersonate both
// services — no SigV4 signing, no real AWS account required.
//
// Both service's operations live under different real-world hostnames
// (oidc.<region>.amazonaws.com vs portal.sso.<region>.amazonaws.com) but
// their paths never collide, so ONE fake server backs both
// AWS_ENDPOINT_URL_SSO_OIDC and AWS_ENDPOINT_URL_SSO.
package awsssofake

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// bearerHeader is the exact header GetRoleCredentials/ListAccounts/
// ListAccountRoles read the SSO access token from — confirmed in
// sso/2019-06-10/service-2.json's AccessTokenType member
// (location=header, locationName=x-amz-sso_bearer_token). Phase B (see
// runs_bedrock.go's resolveBedrockAuth doc comment) will proxy-inject
// exactly this header, so a fake that enforces it documents that contract.
const bearerHeader = "x-amz-sso_bearer_token"

// deviceGrantType is the OAuth device-code grant CreateToken expects during
// `aws sso login --use-device-code`.
const deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

// refreshGrantType is the grant a DISPATCH-TIME renewal uses: wardynd's
// createAWSSSOToken (internal/api/awssso_refresh.go) POSTs it with the stored
// refreshToken, never the device code. Modelled here so that renewal is
// exercised against the same server the login wrote the token on.
const refreshGrantType = "refresh_token"

// RoleCredentials is the fixture GetRoleCredentials returns, matching the
// sso service's RoleCredentials shape (expiration is epoch millis, per the
// ExpirationTimestampType `long` model).
type RoleCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expiration      time.Time
}

// Account is one entitlement: an AWS account this SSO session reaches, and
// the roles it may assume in THAT account. Roles is per-account because the
// real portal's ListAccountRoles is scoped to the account_id it is asked
// about — see handleListAccountRoles.
type Account struct {
	AccountID string
	Roles     []string
}

// Server is the fake sso-oidc + sso portal. Zero value is not usable; use New.
type Server struct {
	httpSrv *httptest.Server

	mu sync.Mutex

	// RegisterClient output, checked (loosely) on later calls.
	clientID     string
	clientSecret string

	// Device-authorization-flow state.
	deviceCode string
	userCode   string
	approved   bool

	// Tokens. accessToken rotates on every successful CreateToken so a test
	// can assert the LATEST token is what's expected.
	accessToken  string
	refreshToken string

	// Fixtures for the account/role + role-credentials lookups. accounts is a
	// LIST because a real SSO session commonly reaches several accounts, and
	// which element lands at index 0 is AWS's choice, not the operator's — the
	// whole of finding 1. SetAccounts lets a test seed that shape.
	accounts []Account
	roleCred RoleCredentials

	// startURLSeen/regionSeen let a test assert the CLI actually round-tripped
	// what the operator configured.
	startURLSeen string

	// bedrockCalls/bedrockModel are the bedrock-runtime stub's observations
	// (bedrock.go): how many model calls landed, and the model id of the last
	// one. Held here rather than in a second struct so /_seen answers the whole
	// walk's question — "was the credential minted, AND was it spent?" — in one
	// round trip.
	bedrockCalls int
	bedrockModel string

	// roleCredsSeen is the account_id/role_name of the LAST GetRoleCredentials
	// call. It is the only place a test can see WHICH identity real botocore
	// actually asked AWS for, which is the whole question behind the account
	// pin — everything upstream of it is Wardyn asserting about itself.
	roleCredsSeen Account
}

// New starts a fake sso-oidc + sso portal server. The account/role and role
// credentials it returns are fixed test fixtures; call AccessToken/Approve to
// drive the device-code flow from a test.
func New() *Server {
	s, h := NewHandler()
	s.httpSrv = httptest.NewServer(h)
	return s
}

// NewHandler returns an UNSTARTED fake plus the handler that serves both
// services, for test/awsssofake/cmd — the on-cluster build, which binds a fixed
// port instead of httptest's ephemeral one. A second constructor rather than an
// exported field: New() must keep starting its own server for every in-process
// caller that already exists, and an unstarted Server's URL() is "".
func NewHandler() (*Server, http.Handler) {
	s := &Server{
		clientID:     randHex(8),
		clientSecret: randHex(16),
		deviceCode:   randHex(16),
		userCode:     "WXYZ-1234",
		accessToken:  "fake-access-token-" + randHex(8),
		refreshToken: "fake-refresh-token-" + randHex(8),
		accounts:     []Account{{AccountID: "111111111111", Roles: []string{"AdministratorAccess"}}},
		roleCred: RoleCredentials{
			AccessKeyID:     "ASIAFAKEFAKEFAKEFAKE",
			SecretAccessKey: "fakeSecretAccessKeyFakeSecretAccessKeyFake",
			SessionToken:    "fake-session-token-" + randHex(24),
			Expiration:      time.Now().Add(1 * time.Hour),
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/client/register", s.handleRegisterClient)
	mux.HandleFunc("/device_authorization", s.handleStartDeviceAuthorization)
	mux.HandleFunc("/token", s.handleCreateToken)
	mux.HandleFunc("/federation/credentials", s.handleGetRoleCredentials)
	mux.HandleFunc("/assignment/accounts", s.handleListAccounts)
	mux.HandleFunc("/assignment/roles", s.handleListAccountRoles)
	// The two arms the ON-CLUSTER fake adds (test/awsssofake/cmd): an
	// observation endpoint, because a test driving a POD cannot call
	// RoleCredentialsSeen() in-process, and a bedrock-runtime stub, because
	// nothing else ever SPENDS the role credentials this portal mints.
	mux.HandleFunc("/_seen", s.handleSeen)
	mux.HandleFunc("/model/", s.handleBedrockRuntime)
	return s, mux
}

// URL is the base URL for BOTH AWS_ENDPOINT_URL_SSO_OIDC and
// AWS_ENDPOINT_URL_SSO — the two services' paths never collide (see package
// doc), so one fake backs both endpoint overrides.
func (s *Server) URL() string {
	if s.httpSrv == nil {
		return "" // NewHandler: the caller owns the listener
	}
	return s.httpSrv.URL
}

// Close shuts down the underlying httptest server.
func (s *Server) Close() {
	if s.httpSrv != nil {
		s.httpSrv.Close()
	}
}

// Approve simulates the human completing the device-code approval (normally
// done by visiting verificationUriComplete in a browser). Until called,
// CreateToken keeps returning authorization_pending, mirroring the real
// device-code flow's poll loop.
func (s *Server) Approve() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.approved = true
}

// AccessToken returns the CURRENT valid access token (post device-code grant
// or post refresh) — the value GetRoleCredentials/ListAccounts/
// ListAccountRoles must see in the bearer header.
func (s *Server) AccessToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accessToken
}

// Account returns the FIRST entitlement — what ListAccounts puts at index 0.
// It is deliberately the same accessor it always was, so a single-account
// caller reads the same fixture; a multi-account test seeds with SetAccounts
// and names the element it means.
func (s *Server) Account() Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accounts[0]
}

// SetAccounts replaces the entitlement fixture. At least one account is
// required: a session that reaches none is not a shape this fake models (the
// helper's own empty-list arm is exercised against an unreachable portal).
func (s *Server) SetAccounts(accounts []Account) {
	if len(accounts) == 0 {
		panic("awsssofake: SetAccounts needs at least one account")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts = accounts
}

// accountsSnapshot is the locked read every handler below makes.
func (s *Server) accountsSnapshot() []Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accounts
}

// RoleCredentialsSeen returns the account_id and role_name of the last
// GetRoleCredentials call — what the SDK actually asked AWS to mint.
func (s *Server) RoleCredentialsSeen() Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.roleCredsSeen
}

// StartURLSeen returns the sso_start_url the CLI sent to
// StartDeviceAuthorization, once the flow has run at least once (empty
// otherwise).
func (s *Server) StartURLSeen() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startURLSeen
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeOIDCError writes an sso-oidc modeled-exception response. botocore's
// rest-json error parser reads the exception TYPE from the x-amzn-errortype
// response header (falling back to a body "code"/"__type" field neither of
// these OAuth-shaped bodies carries), then parses the body against that
// shape's members — here always {error, error_description}, matching
// AuthorizationPendingException/InvalidGrantException/etc in
// sso-oidc/2019-06-10/service-2.json (all httpStatusCode 400).
func writeOIDCError(w http.ResponseWriter, errType, oauthCode, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("x-amzn-errortype", errType)
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":             oauthCode,
		"error_description": description,
	})
}

// registerClientRequest/Response mirror RegisterClientRequest/Response.
func (s *Server) handleRegisterClient(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	// Request body (clientName/clientType) isn't validated — this fake
	// exists to prove the wire shape, not to police the CLI's own request.
	now := time.Now()
	writeJSON(w, http.StatusOK, map[string]any{
		"clientId":              s.clientID,
		"clientSecret":          s.clientSecret,
		"clientIdIssuedAt":      now.Unix(),
		"clientSecretExpiresAt": now.Add(90 * 24 * time.Hour).Unix(),
	})
}

func (s *Server) handleStartDeviceAuthorization(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var req struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
		StartURL     string `json:"startUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOIDCError(w, "InvalidRequestException", "invalid_request", err.Error())
		return
	}
	if req.ClientID != s.clientID || req.ClientSecret != s.clientSecret {
		writeOIDCError(w, "InvalidClientException", "invalid_client", "unknown client")
		return
	}
	s.mu.Lock()
	s.startURLSeen = req.StartURL
	deviceCode, userCode := s.deviceCode, s.userCode
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"deviceCode":              deviceCode,
		"userCode":                userCode,
		"verificationUri":         s.URL() + "/verify",
		"verificationUriComplete": s.URL() + "/verify?user_code=" + userCode,
		"expiresIn":               900,
		// interval=1: the real device-code flow's polling backoff (default
		// 5s) would make a test wait many seconds for Approve() to land
		// between polls; a 1s interval keeps the fake fast without changing
		// protocol shape.
		"interval": 1,
	})
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var req struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
		GrantType    string `json:"grantType"`
		DeviceCode   string `json:"deviceCode"`
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOIDCError(w, "InvalidRequestException", "invalid_request", err.Error())
		return
	}
	if req.ClientID != s.clientID || req.ClientSecret != s.clientSecret {
		writeOIDCError(w, "InvalidClientException", "invalid_client", "unknown client")
		return
	}

	switch req.GrantType {
	case deviceGrantType:
		s.mu.Lock()
		if req.DeviceCode != s.deviceCode {
			s.mu.Unlock()
			writeOIDCError(w, "InvalidGrantException", "invalid_grant", "unknown device code")
			return
		}
		if !s.approved {
			s.mu.Unlock()
			writeOIDCError(w, "AuthorizationPendingException", "authorization_pending", "device authorization is still pending user approval")
			return
		}
		// Rotate on issuance so checkBearer only accepts the token this
		// login just handed out.
		s.accessToken = "fake-access-token-" + randHex(8)
		access := s.accessToken
		refresh := s.refreshToken
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"accessToken":  access,
			"tokenType":    "Bearer",
			"expiresIn":    3600,
			"refreshToken": refresh,
		})
		return

	case refreshGrantType:
		s.mu.Lock()
		if req.RefreshToken == "" || req.RefreshToken != s.refreshToken {
			s.mu.Unlock()
			// invalid_grant, which is exactly what a CONSUMED or unknown refresh
			// token gets from the real service — and the code Wardyn classifies as
			// "spent" (awsSSOErrorIsSpent), stopping a fleet from re-trying a dead
			// grant. A fake that answered 200 here would let a replay look healthy.
			writeOIDCError(w, "InvalidGrantException", "invalid_grant", "unknown or already-consumed refresh token")
			return
		}
		// BOTH tokens rotate. The real service rotates the refresh token on every
		// redemption, and refreshAWSSSOBlob's `rotated` audit arm + its
		// store-the-new-pair path are only exercised when the token actually
		// moves — a fake that returned the same refreshToken would leave the half
		// that persists the rotation untested.
		s.accessToken = "fake-access-token-" + randHex(8)
		s.refreshToken = "fake-refresh-token-" + randHex(8)
		access, refresh := s.accessToken, s.refreshToken
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"accessToken":  access,
			"tokenType":    "Bearer",
			"expiresIn":    3600,
			"refreshToken": refresh,
		})
		return

	default:
		writeOIDCError(w, "UnsupportedGrantTypeException", "unsupported_grant_type", "grantType "+req.GrantType+" not supported by this fake")
	}
}

// handleSeen is the fake's observation endpoint — NOT an AWS operation. It
// exposes RoleCredentialsSeen() (and the bedrock stub's counters) as JSON so a
// walk driving the fake as a POD can read the one fact that matters: WHICH
// identity real botocore asked the portal to mint. In-process callers keep
// using the accessors; this is the same answer across a process boundary.
//
// Deliberately unauthenticated: the fake holds no real credential, it runs only
// on a throwaway cluster, and a bearer check here would make the walk carry a
// token it has no other reason to know.
func (s *Server) handleSeen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	role := ""
	if len(s.roleCredsSeen.Roles) > 0 {
		role = s.roleCredsSeen.Roles[0]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id":    s.roleCredsSeen.AccountID,
		"role_name":     role,
		"start_url":     s.startURLSeen,
		"bedrock_calls": s.bedrockCalls,
		"bedrock_model": s.bedrockModel,
	})
}

func (s *Server) handleGetRoleCredentials(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	if !s.checkBearer(w, r) {
		return
	}
	s.mu.Lock()
	s.roleCredsSeen = Account{
		AccountID: r.URL.Query().Get("account_id"),
		Roles:     []string{r.URL.Query().Get("role_name")},
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"roleCredentials": map[string]any{
			"accessKeyId":     s.roleCred.AccessKeyID,
			"secretAccessKey": s.roleCred.SecretAccessKey,
			"sessionToken":    s.roleCred.SessionToken,
			"expiration":      s.roleCred.Expiration.UnixMilli(),
		},
	})
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	if !s.checkBearer(w, r) {
		return
	}
	list := []map[string]any{}
	for _, a := range s.accountsSnapshot() {
		list = append(list, map[string]any{
			"accountId":    a.AccountID,
			"accountName":  "fake-account-" + a.AccountID,
			"emailAddress": "fake@example.com",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"accountList": list})
}

func (s *Server) handleListAccountRoles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	if !s.checkBearer(w, r) {
		return
	}
	// SCOPED TO THE REQUESTED ACCOUNT, and an error for one this session does
	// not reach. The real portal answers per account_id (the parameter has been
	// on the wire since 2019); a fake that ignored it answered account A's
	// roles to a question about account B, so a caller VERIFYING that a pinned
	// role exists in a pinned account would have been verifying nothing. The
	// unentitled arm is an error rather than an empty roleList for the same
	// reason: "not entitled" and "entitled to nothing" are different answers,
	// and only the first one means the pin is wrong.
	//
	// 403 ForbiddenException is the real portal's shape for an account the
	// session is not entitled to. Wardyn's helper treats every non-2xx below
	// 500 identically ("not entitled"), so this is documentation rather than
	// behaviour here — but a fake that models the wrong status is a fake
	// somebody will one day believe. Exercising a real two-entitlement tenant
	// remains owner-hardware-only (see the release's Known gaps).
	wanted := r.URL.Query().Get("account_id")
	for _, a := range s.accountsSnapshot() {
		if a.AccountID != wanted {
			continue
		}
		list := []map[string]any{}
		for _, role := range a.Roles {
			list = append(list, map[string]any{"roleName": role, "accountId": a.AccountID})
		}
		writeJSON(w, http.StatusOK, map[string]any{"roleList": list})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("x-amzn-errortype", "ForbiddenException")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"message": "this session is not entitled to account " + wanted,
	})
}

// checkBearer enforces the x-amz-sso_bearer_token header matches the
// currently-issued access token — the exact contract Phase B's proxy
// injection (runs_bedrock.go doc comment) will need to satisfy. Writes a 401
// modeled UnauthorizedException and returns false on mismatch/absence.
func (s *Server) checkBearer(w http.ResponseWriter, r *http.Request) bool {
	got := r.Header.Get(bearerHeader)
	s.mu.Lock()
	want := s.accessToken
	s.mu.Unlock()
	if got == "" || got != want {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-amzn-errortype", "UnauthorizedException")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "missing or invalid " + bearerHeader})
		return false
	}
	return true
}
