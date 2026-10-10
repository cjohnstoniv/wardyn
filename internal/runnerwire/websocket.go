// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
)

// WebSocket carries exactly one encoded frame in each binary message.
type WebSocket struct{ conn *websocket.Conn }

// NewWebSocket applies the protocol's outer message bound before any reads.
func NewWebSocket(conn *websocket.Conn) *WebSocket {
	conn.SetReadLimit(HeaderLen + MaxControlFrame)
	return &WebSocket{conn: conn}
}

func (c *WebSocket) ReadFrame(ctx context.Context) (Frame, error) {
	typ, b, err := c.conn.Read(ctx)
	if err != nil {
		return Frame{}, err
	}
	if typ != websocket.MessageBinary {
		return Frame{}, fmt.Errorf("%w: expected a binary WebSocket message", ErrBadFrame)
	}
	return Decode(b)
}

func (c *WebSocket) WriteFrame(ctx context.Context, f Frame) error {
	b, err := f.Encode(nil)
	if err != nil {
		return err
	}
	return c.conn.Write(ctx, websocket.MessageBinary, b)
}

func (c *WebSocket) Close() error { return c.conn.CloseNow() }

// StreamURL refuses credentials, query/fragment ambiguity and non-TLS remote
// endpoints. Loopback HTTP is the local development transport.
func StreamURL(orgURL, runnerID string) (string, error) {
	id, err := uuid.Parse(runnerID)
	if err != nil || id.String() != runnerID {
		return "", fmt.Errorf("runnerwire: invalid runner id")
	}
	if err := federation.CheckOrgURL(orgURL); err != nil {
		return "", err
	}
	u, _ := url.Parse(strings.TrimSpace(orgURL))
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("runnerwire: the organisation URL requires HTTPS")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/runners/" + runnerID + "/stream"
	u.RawPath = ""
	return u.String(), nil
}

// DialWebSocket uses the HTTP transport's TLS roots and HTTPS_PROXY support.
// Redirects are always refused, including when a caller supplies a client.
func DialWebSocket(ctx context.Context, orgURL, runnerID string, client *http.Client) (*WebSocket, error) {
	address, err := StreamURL(orgURL, runnerID)
	if err != nil {
		return nil, err
	}
	hc := http.Client{}
	if client != nil {
		hc = *client
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c, resp, err := websocket.Dial(ctx, address, &websocket.DialOptions{HTTPClient: &hc, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, err
	}
	return NewWebSocket(c), nil
}
