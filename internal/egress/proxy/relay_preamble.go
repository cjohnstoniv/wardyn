// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strings"
)

const (
	// relayPreambleMagic and relayPreambleVersion open the 34-byte preamble a keyed proxy writes first on
	// a connection to the control-plane address, before TLS. 0x57 is no TLS record type, so the relay
	// tells it from a ClientHello (0x16) on the first byte.
	relayPreambleMagic   = 0x57
	relayPreambleVersion = 0x01
	relayKeyLen          = 32
)

// relayKeyValue is the run's relay key as written in the config (base64). It prints as a placeholder
// under every fmt verb and as a placeholder in slog, so a config dumped by accident never carries it.
type relayKeyValue string

func (relayKeyValue) String() string               { return "[redacted]" }
func (relayKeyValue) GoString() string             { return "[redacted]" }
func (relayKeyValue) LogValue() slog.Value         { return slog.StringValue("[redacted]") }
func (k relayKeyValue) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, "[redacted]") }

// errRelayKey is every relay_key refusal: it never carries the value or a byte of it.
var errRelayKey = errors.New("config: relay_key must be base64 of exactly 32 bytes")

// decode returns the key's 32 bytes, or errRelayKey when it is not base64 of exactly 32 bytes.
func (k relayKeyValue) decode() ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(string(k))
	if err != nil || len(b) != relayKeyLen {
		return nil, errRelayKey
	}
	return b, nil
}

// validateRelayKey refuses a relay_key that is set and not a 32-byte base64 value. Unset is valid.
func (c *Config) validateRelayKey() error {
	if c.RelayKey == "" {
		return nil
	}
	_, err := c.RelayKey.decode()
	return err
}

// relayPreamble is what the control-plane client writes on a connection to addr before TLS. dial, when
// set, replaces the transport's own dialer beneath the preamble (the tests use it to resolve a name).
type relayPreamble struct {
	addr string
	key  []byte
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Format keeps the key out of any dump of the preamble.
func (relayPreamble) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, "[redacted]") }

// relayPreambleFor returns the preamble for a config, or nil when it has no relay_key.
func relayPreambleFor(cfg *Config) (*relayPreamble, error) {
	if cfg.RelayKey == "" {
		return nil, nil
	}
	key, err := cfg.RelayKey.decode()
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(cfg.ControlPlaneURL)
	if err != nil {
		return nil, fmt.Errorf("config: control_plane_url: %w", err)
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if strings.EqualFold(u.Scheme, "http") {
			port = "80"
		}
	}
	return &relayPreamble{addr: net.JoinHostPort(u.Hostname(), port), key: key}, nil
}

// wrap returns a DialContext that dials through base and writes the preamble right after connect when,
// and only when, the dialled address is the control-plane address. Every other address gets none.
func (p *relayPreamble) wrap(base func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if p.dial != nil {
		base = p.dial
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := base(ctx, network, addr)
		if err != nil || !strings.EqualFold(addr, p.addr) {
			return c, err
		}
		msg := make([]byte, 0, 2+relayKeyLen)
		msg = append(append(msg, relayPreambleMagic, relayPreambleVersion), p.key...)
		if _, err := c.Write(msg); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("relay preamble: %w", err)
		}
		return c, nil
	}
}
