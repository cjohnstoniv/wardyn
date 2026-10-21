// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// CLI sign-in (wardyn login): the browser approves a request the CLI made, and the CLI then holds a
// CLI session (a wdc_ refresh secret bound to an Ed25519 device key) that renews a 1-hour wdn_ access
// token. The session secret and the device key live on the person's computer, in the keychain or, on
// Linux with no keychain, in a 0600 file; see docs/operations/api-tokens.md.

// API token kinds (api_tokens.kind). A personal token is "api"; the two CLI kinds carry the CLI session
// they belong to and end with it.
const (
	APITokenKindAPI       = "api"
	APITokenKindCLIAccess = "cli_access" // 1 h, renewed by a refresh, publishes the role user
	APITokenKindCLIAdmin  = "cli_admin"  // wardyn login --admin: 1 h, the real role, never renewed
)

// APITokenKinds is the closed set api_tokens.kind admits.
var APITokenKinds = []string{APITokenKindAPI, APITokenKindCLIAccess, APITokenKindCLIAdmin}

// CLI sign-in modes (cli_sign_ins.mode).
const (
	CLISignInModeLoopback = "loopback" // the approve page sends the browser to http://127.0.0.1:<port>/callback
	CLISignInModePaste    = "paste"    // --no-browser: the page shows the code to paste into the terminal
)

// CLISignInModes is the closed set cli_sign_ins.mode admits.
var CLISignInModes = []string{CLISignInModeLoopback, CLISignInModePaste}

// A sign-in request's decision (cli_sign_ins.decision); NULL while it waits for one.
const (
	CLISignInApproved = "approved"
	CLISignInDenied   = "denied"
)

// CLISignInDecisions is the closed set cli_sign_ins.decision admits.
var CLISignInDecisions = []string{CLISignInApproved, CLISignInDenied}

// CLI credential stores the CLI reports (cli_sign_ins.store, cli_sessions.store). The report is the
// CLI's own word: the server applies the file store's cap and WARDYN_CLI_FILE_STORE=off from it, which
// guards against accident, not against a modified CLI.
const (
	CLIStoreKeychain = "keychain"
	CLIStoreFile     = "file"
)

// CLIStores is the closed set the store columns admit.
var CLIStores = []string{CLIStoreKeychain, CLIStoreFile}

// Why a CLI session ended (cli_sessions.end_reason).
const (
	CLISessionEndSignedOut     = "signed_out"     // wardyn logout, or Sign out on its row under Signed-in CLIs
	CLISessionEndRevoked       = "revoked"        // a person revoke, revoke-all, SCIM suspension or identity unbind
	CLISessionEndSessionsCut   = "sessions_cut"   // Sign out everywhere, the console's Sign out, back-channel logout, a SCIM group cut
	CLISessionEndReuseDetected = "reuse_detected" // a refresh presented an old secret with a valid device proof
	CLISessionEndExpired       = "expired"        // past its absolute lifetime
	CLISessionEndIdle          = "idle"           // no refresh within the idle lifetime
	CLISessionEndDeactivated   = "deactivated"    // the owner's identity was deactivated
)

// CLISessionEndReasons is the closed set cli_sessions.end_reason admits.
var CLISessionEndReasons = []string{CLISessionEndSignedOut, CLISessionEndRevoked, CLISessionEndSessionsCut,
	CLISessionEndReuseDetected, CLISessionEndExpired, CLISessionEndIdle, CLISessionEndDeactivated}

// CLI wire limits and lifetimes the server enforces.
const (
	CLISignInRequestLife   = 10 * time.Minute // a sign-in request, from create to approve
	CLISignInLoopbackCode  = 60 * time.Second // K after Approve, loopback mode
	CLISignInPasteCode     = 5 * time.Minute  // K after Approve, paste-back mode
	CLIAccessTokenLife     = time.Hour        // cli_access and cli_admin, before WARDYN_API_TOKEN_MAX_TTL
	CLIFileStoreSessionCap = 7 * 24 * time.Hour
	CLIProofSkew           = 5 * time.Minute // a device proof's time may be this far from the server's
	CLISessionSecretPrefix = "wdc_"
)

// CLISignInCreateRequest is the anonymous POST /api/v1/cli/sign-ins body.
type CLISignInCreateRequest struct {
	// Challenge is base64url(SHA-256(verifier)) with no padding: PKCE S256, the only method.
	Challenge string `json:"challenge"`
	// Port is the CLI's loopback callback port; required for loopback, absent for paste.
	Port int `json:"port,omitempty"`
	// Machine is the computer's name as the CLI reads it; the approve page shows it.
	Machine string `json:"machine"`
	// PublicKey is the device key's Ed25519 public half, base64url with no padding (32 bytes).
	PublicKey string `json:"public_key"`
	Mode      string `json:"mode"`  // CLISignInModeLoopback or CLISignInModePaste
	Store     string `json:"store"` // CLIStoreKeychain or CLIStoreFile
	// Admin asks for a cli_admin step-up (wardyn login --admin). SessionID is then required: the live
	// CLI session on this computer, whose device key signs the token call and whose end ends the token.
	Admin     bool       `json:"admin,omitempty"`
	SessionID *uuid.UUID `json:"session_id,omitempty"`
}

// CLISignInCreateResponse answers a created sign-in request. The CLI opens
// <org>/cli/sign-in?id=<ID> and prints UserCode.
type CLISignInCreateResponse struct {
	ID        uuid.UUID `json:"id"`
	UserCode  string    `json:"user_code"`
	ExpiresIn int       `json:"expires_in"` // seconds; 600
}

// CLISignInView is GET /api/v1/me/cli-sign-ins/{id}: what the approve page shows. The lifetimes are
// the operator's actual ones for this request's store (the file store is capped at 7 days).
type CLISignInView struct {
	ID       uuid.UUID `json:"id"`
	Machine  string    `json:"machine"`
	UserCode string    `json:"user_code"`
	Mode     string    `json:"mode"`
	Store    string    `json:"store"`
	Admin    bool      `json:"admin"`
	// RequestAddress is where the sign-in request came from, ApproverAddress where this page's request
	// came from, both read the same way (WARDYN_TRUSTED_PROXIES). AddressMatch is advisory only: behind
	// an unlisted proxy both read the proxy and always match.
	RequestAddress  string    `json:"request_address"`
	ApproverAddress string    `json:"approver_address"`
	AddressMatch    bool      `json:"address_match"`
	CreatedAt       time.Time `json:"created_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	// SessionTTLSeconds and SessionIdleSeconds are the session's absolute and idle lifetimes;
	// AccessTTLSeconds the access (or, for Admin, the admin) token's.
	SessionTTLSeconds  int64 `json:"session_ttl_seconds"`
	SessionIdleSeconds int64 `json:"session_idle_seconds"`
	AccessTTLSeconds   int64 `json:"access_ttl_seconds"`
}

// CLISignInApproveResponse answers POST /api/v1/me/cli-sign-ins/{id}/approve. Code is K, single use:
// the console page navigates the browser to RedirectURL (loopback) or shows Code to paste (paste).
type CLISignInApproveResponse struct {
	Code          string `json:"code"`
	CodeExpiresIn int    `json:"code_expires_in"` // seconds: 60 loopback, 300 paste
	RedirectURL   string `json:"redirect_url,omitempty"`
}

// CLISignInDenyResponse answers POST /api/v1/me/cli-sign-ins/{id}/deny. For loopback the console page
// navigates the browser to RedirectURL (…/callback?id=<id>&error=denied), so the CLI can say the
// sign-in was denied; paste mode has none.
type CLISignInDenyResponse struct {
	RedirectURL string `json:"redirect_url,omitempty"`
}

// CLIProof is the device-key proof on the token and refresh calls: an Ed25519 signature, base64url
// with no padding, over CLIProofMessage, made at Time (Unix seconds, within CLIProofSkew).
type CLIProof struct {
	Time      int64  `json:"time"`
	Signature string `json:"signature"`
}

// CLIProofMessage is the exact byte string a CLIProof signs. origin is the organisation's URL as the CLI
// saved it, normalised to scheme://host[:port][base path] with no trailing slash; route is the API path
// from /api/v1 (for example /api/v1/cli/sessions/<id>/refresh); id is the sign-in id on the token call
// and the session id on a refresh; secret is the code K on the token call and the wdc_ secret on a
// refresh. Only the secret's SHA-256 is signed, so the proof never carries it.
func CLIProofMessage(origin, route string, id uuid.UUID, secret string, at int64) []byte {
	sum := sha256.Sum256([]byte(secret))
	return []byte("wardyn-cli-proof/v1\n" + origin + "\n" + route + "\n" + id.String() + "\n" +
		hex.EncodeToString(sum[:]) + "\n" + strconv.FormatInt(at, 10))
}

// CLIOrigin is u as the origin a device proof signs: scheme, host and base path, with the scheme and host
// lower-cased, the default port (443 for https, 80 for http) dropped and no trailing slash. The CLI signs
// over the URL it dials in this form, and the server keeps its own origin in it, so the same organisation
// written two ways (an explicit :443, a capital letter in the host) is one origin.
func CLIOrigin(u *url.URL) string {
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host + strings.TrimRight(u.EscapedPath(), "/")
}

// CLISignInHealthz is what /healthz says about CLI sign-in. CLISignInOrigin is the origin the server
// checks device proofs against (CLIOrigin form). The CLI only compares it with the URL it dials and
// refuses on a difference; it never signs over it, so a look-alike server cannot obtain a proof for the
// origin it advertises.
type CLISignInHealthz struct {
	CLISignIn       bool   `json:"cli_sign_in"`
	CLIPasteSignIn  bool   `json:"cli_paste_sign_in"`
	CLISignInOrigin string `json:"cli_sign_in_origin,omitempty"`
}

// CLISignInTokenRequest is the anonymous POST /api/v1/cli/sign-ins/{id}/token body.
type CLISignInTokenRequest struct {
	Code     string   `json:"code"`     // K
	Verifier string   `json:"verifier"` // the PKCE verifier whose S256 is the request's challenge
	Proof    CLIProof `json:"proof"`
}

// CLISessionRefreshRequest is the anonymous POST /api/v1/cli/sessions/{id}/refresh body.
type CLISessionRefreshRequest struct {
	Secret string   `json:"secret"` // the current wdc_ secret
	Proof  CLIProof `json:"proof"`
}

// CLISessionGrant answers the token call and every refresh. A sign-in and a refresh return a new
// Secret (the old one is spent: presenting it again ends the session) and a cli_access token; an
// admin step-up returns only the cli_admin token, with no Secret.
type CLISessionGrant struct {
	SessionID        uuid.UUID `json:"session_id"`
	Secret           string    `json:"secret,omitempty"`
	AccessToken      string    `json:"access_token"`
	Kind             string    `json:"kind"` // APITokenKindCLIAccess or APITokenKindCLIAdmin
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	SessionExpiresAt time.Time `json:"session_expires_at"`
	IdleExpiresAt    time.Time `json:"idle_expires_at"`
	Store            string    `json:"store"`
}

// CLISession is one row of GET /api/v1/me/cli-sessions ("Signed-in CLIs"): live sessions only, never a
// secret. AdminExpiresAt is set while a cli_admin step-up on that computer is live.
type CLISession struct {
	ID             uuid.UUID  `json:"id"`
	Machine        string     `json:"machine"`
	Store          string     `json:"store"`
	CreatedAt      time.Time  `json:"created_at"`
	LastRefreshAt  time.Time  `json:"last_refresh_at"`
	ExpiresAt      time.Time  `json:"expires_at"`
	IdleExpiresAt  time.Time  `json:"idle_expires_at"`
	AdminExpiresAt *time.Time `json:"admin_expires_at,omitempty"`
}
