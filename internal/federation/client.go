// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package federation is the laptop half of hybrid enrolment
// (docs/design/0.8/PLAN.md, "Hybrid enrolment and audit federation"): the
// client for the organisation's device routes (internal/api/devices_auth.go)
// and the forwarder that pushes this daemon's own chained audit rows upward
// from a durable cursor. It READS the local audit table and never records into
// the chain on anyone's behalf; the two rows it writes are its own
// (device.local.enrol at boot, device.local.revoke here).
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

// Credential is what the laptop keeps in its secret store after enrolling: the
// device id the organisation's routes are keyed on, the wdd_ bearer, and the
// SHA-256 of the enrolment token that bought them. The last is how boot tells
// a fresh token (re-enrol) from the spent one MDM leaves in place (keep).
type Credential struct {
	DeviceID             uuid.UUID `json:"device_id"`
	Token                string    `json:"token"`
	EnrolmentTokenSHA256 string    `json:"enrolment_token_sha256"`
}

// TokenSHA256 is hex(sha256(token)), the form Credential records.
func TokenSHA256(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// StatusError is a non-2xx answer from the organisation. RetryAfter is the
// parsed Retry-After header (seconds or HTTP-date form), zero when absent or
// unparseable.
type StatusError struct {
	Code       int
	RetryAfter time.Duration
	Message    string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("organisation answered %d: %s", e.Code, e.Message)
}

// Revoked reports the definitive "this credential is dead" answers: 401 from
// deviceAuth (revoked and unknown are deliberately one answer there) and 410.
func (e *StatusError) Revoked() bool {
	return e.Code == http.StatusUnauthorized || e.Code == http.StatusGone
}

// Client calls the organisation's device routes.
type Client struct {
	base string
	http *http.Client
}

// NewClient returns a client for orgURL. Its transport is the shared
// http.DefaultTransport, which wardynd patches at boot with
// WARDYN_DAEMON_PROXY_URL and WARDYN_TRUSTED_CA_FILE — the same path the audit
// webhook sink rides. Redirects are refused: the bearer is for this URL only.
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
			Message: strings.TrimSpace(string(msg))}
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
