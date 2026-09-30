// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package federation is the laptop half of hybrid enrolment: the client for
// the organisation's device routes, and the forwarder pushing this daemon's
// audit rows upward. It never writes into the chain on anyone else's behalf.
package federation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// requestTimeout bounds one call to the organisation. A full ingest batch is
// at most 8 MiB (maxDeviceIngestBytes), which this allows over a slow link.
const requestTimeout = 60 * time.Second

// Credential is what the laptop keeps in its secret store after enrolling:
// device id, wdd_ bearer, and the enrolment token's SHA-256 (tells a fresh
// token from the spent one MDM leaves behind). ResetPending, persisted true
// only on a fresh credential, makes the post-enrolment reset resumable across
// a crash without re-spending the token or clearing a later, genuine revocation.
// OrgURLSHA256 binds the credential to the WARDYN_ORG_URL it was enrolled at, so
// the bearer is never sent to a different host; empty on a credential stored
// before that field existed (bootHybrid adopts the configured URL for it).
type Credential struct {
	DeviceID             uuid.UUID `json:"device_id"`
	Token                string    `json:"token"`
	EnrolmentTokenSHA256 string    `json:"enrolment_token_sha256"`
	OrgURLSHA256         string    `json:"org_url_sha256,omitempty"`
	ResetPending         bool      `json:"reset_pending,omitempty"`
	Name                 string    `json:"name,omitempty"`
}

// OrgURLSHA256 is hex(sha256(url)) of the organisation URL as NewClient reads
// it (surrounding space and trailing slashes dropped), the form Credential records.
func OrgURLSHA256(orgURL string) string {
	sum := sha256.Sum256([]byte(strings.TrimRight(strings.TrimSpace(orgURL), "/")))
	return hex.EncodeToString(sum[:])
}

// TokenSHA256 is hex(sha256(token)), the form Credential records.
func TokenSHA256(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// StatusError is a non-2xx answer from the organisation. RetryAfter is the
// parsed Retry-After header; WWWAuthenticate lets Revoked tell the
// organisation's own 401 from one injected elsewhere on the path.
type StatusError struct {
	Code            int
	RetryAfter      time.Duration
	Message         string
	WWWAuthenticate string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("organisation answered %d: %s", e.Code, e.Message)
}

// Revoked reports the definitive "this credential is dead" answers: 410, and
// a 401 carrying deviceAuth's own realm (revoked and unknown are deliberately
// one answer there). A bare 401 without that realm is not revocation.
func (e *StatusError) Revoked() bool {
	return e.Code == http.StatusGone ||
		(e.Code == http.StatusUnauthorized && strings.Contains(e.WWWAuthenticate, `realm="wardyn-device"`))
}

// Client calls the organisation's device routes.
type Client struct {
	base string
	http *http.Client
}

// NewClient returns a client for orgURL, using the shared http.DefaultTransport
// (patched at boot by wardynd). Redirects are refused: the bearer is for this URL only.
func NewClient(orgURL string) *Client {
	return &Client{
		base: strings.TrimRight(orgURL, "/"),
		http: &http.Client{
			Timeout:       requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Enrol trades a single-use enrolment token for this device's credential.
func (c *Client) Enrol(ctx context.Context, enrolmentToken string) (types.DeviceEnrolResponse, error) {
	var out types.DeviceEnrolResponse
	err := c.post(ctx, "/api/v1/devices/enrol", "", types.DeviceEnrolRequest{Token: enrolmentToken}, http.StatusCreated, &out)
	return out, err
}

// Push sends one batch of this daemon's chained rows, oldest first, and
// returns the organisation's recorded cursor for the device.
func (c *Client) Push(ctx context.Context, cred Credential, rows []types.FederatedAuditEvent) (int64, error) {
	var ack types.DeviceAck
	err := c.post(ctx, "/api/v1/devices/"+cred.DeviceID.String()+"/audit", cred.Token, rows, http.StatusOK, &ack)
	return ack.AckedSeq, err
}

// Heartbeat is the idle keepalive; it answers the same recorded cursor.
func (c *Client) Heartbeat(ctx context.Context, cred Credential) (int64, error) {
	var ack types.DeviceAck
	err := c.post(ctx, "/api/v1/devices/"+cred.DeviceID.String()+"/heartbeat", cred.Token, struct{}{}, http.StatusOK, &ack)
	return ack.AckedSeq, err
}

func (c *Client) post(ctx context.Context, path, bearer string, in any, want int, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &StatusError{Code: resp.StatusCode, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			Message: strings.TrimSpace(string(msg)), WWWAuthenticate: resp.Header.Get("WWW-Authenticate")}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

func parseRetryAfter(h string) time.Duration {
	if secs, err := strconv.Atoi(h); err == nil {
		return time.Duration(max(secs, 0)) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		return max(time.Until(t), 0)
	}
	return 0
}
