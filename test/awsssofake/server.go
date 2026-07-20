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

// RoleCredentials is the fixture GetRoleCredentials returns, matching the
// sso service's RoleCredentials shape (expiration is epoch millis, per the
// ExpirationTimestampType `long` model).
type RoleCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expiration      time.Time
}

// Account is one entry ListAccounts / the resolved account for ListAccountRoles.
type Account struct {
	AccountID string
	RoleName  string
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

	// Fixtures for the account/role + role-credentials lookups.
	account  Account
	roleCred RoleCredentials

	// startURLSeen/regionSeen let a test assert the CLI actually round-tripped
	// what the operator configured.
	startURLSeen string
}

// New starts a fake sso-oidc + sso portal server. The account/role and role
// credentials it returns are fixed test fixtures; call AccessToken/Approve to
// drive the device-code flow from a test.
func New() *Server {
	s := &Server{
		clientID:     randHex(8),
		clientSecret: randHex(16),
		deviceCode:   randHex(16),
		userCode:     "WXYZ-1234",
		accessToken:  "fake-access-token-" + randHex(8),
		refreshToken: "fake-refresh-token-" + randHex(8),
		account:      Account{AccountID: "111111111111", RoleName: "AdministratorAccess"},
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
	s.httpSrv = httptest.NewServer(mux)
	return s
}

// URL is the base URL for BOTH AWS_ENDPOINT_URL_SSO_OIDC and
// AWS_ENDPOINT_URL_SSO — the two services' paths never collide (see package
// doc), so one fake backs both endpoint overrides.
func (s *Server) URL() string { return s.httpSrv.URL }

// Close shuts down the underlying httptest server.
func (s *Server) Close() { s.httpSrv.Close() }

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

// Account returns the fixed account/role fixture ListAccounts /
// ListAccountRoles resolve to.
func (s *Server) Account() Account { return s.account }

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

	default:
		writeOIDCError(w, "UnsupportedGrantTypeException", "unsupported_grant_type", "grantType "+req.GrantType+" not supported by this fake")
	}
}

func (s *Server) handleGetRoleCredentials(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	if !s.checkBearer(w, r) {
		return
	}
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
	writeJSON(w, http.StatusOK, map[string]any{
		"accountList": []map[string]any{
			{"accountId": s.account.AccountID, "accountName": "fake-account", "emailAddress": "fake@example.com"},
		},
	})
}

func (s *Server) handleListAccountRoles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	if !s.checkBearer(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"roleList": []map[string]any{
			{"roleName": s.account.RoleName, "accountId": s.account.AccountID},
		},
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
