// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// RunnerHooks joins the transport to the runner's production execution and
// offline state. All handlers except Online/StreamOffline are required. Missing adapters
// never become an empty STATE followed by a fabricated READY.
type RunnerHooks struct {
	Call func(context.Context, *Peer, uint32, string, json.RawMessage) (any, error)
	Open func(*Peer, *Stream, Open) error
	// Clock anchors org time to the local monotonic HELLO receipt instant.
	// It runs after binding validation and before any lease or execution hook.
	Clock func(Hello, time.Time) error
	// Pending applies actions oldest first and durably records each result.
	// It sends action_result through peer only after recording that result.
	Pending func(context.Context, *Peer, []PendingAction) error
	// StatePages emits one fresh inventory as bounded ordered pages through
	// send, ending with Final. Each page is a separate STATE frame.
	StatePages func(ctx context.Context, send func(StatePage) error) error
	// Replay sends the durable spool, completing before READY is sent.
	Replay func(context.Context, *Peer) error
	// BeforeReady establishes local execution admission before the org can
	// send work. On resume it runs before the peer read loop starts, so it
	// must not wait for replies or emit byte streams.
	BeforeReady func(context.Context, *Peer, bool) error
	Lease       func(context.Context, Lease) error
	Revoked     func(context.Context) error
	Offline     func(context.Context)
	Online      func(*Peer, bool)
	// ProxyImage, when set, is told the proxy image the organisation named in AUTH_OK (never "").
	ProxyImage func(string)
	// LocalAttach, when set, is told on every connect whether the organisation's AUTH_OK allows local
	// attach (false from an organisation that does not say).
	LocalAttach func(bool)
	// StreamOffline releases per-connection PTY/exec resources, not sandboxes.
	StreamOffline func(*Peer)
}

// ClientConfig holds the registered identity and the shared HTTP transport.
// Dial, when supplied, is the in-memory conformance/test transport; production
// uses DialWebSocket with TLS roots, proxy support and redirect refusal.
type ClientConfig struct {
	OrgURL, RunnerID string
	PrivateKey       ed25519.PrivateKey
	HTTPClient       *http.Client
	Dial             func(context.Context) (Conn, error)
	Hooks            RunnerHooks
	// LinkChanged, when set, is told each time a session comes up (reason "") or an attempt ends (reason
	// is why, "" when it ended without one). It must not block.
	LinkChanged func(up bool, reason string)
}

// Client owns one runner's reconnect loop. Run must not be called concurrently.
type Client struct {
	cfg     ClientConfig
	peer    *Peer
	session string
	ended   time.Time
	ready   bool
	link    linkState
}

// offlineLogEvery is how often a runner with no session up says it is still offline, whatever its
// connection attempt is doing: a dial, handshake or hook that is waiting is never silent for longer.
var offlineLogEvery = time.Minute

// linkState is what the offline log reads: whether a session is up, since when it has not been, and
// why the last attempt ended.
type linkState struct {
	mu     sync.Mutex
	up     bool
	since  time.Time
	reason string
}

func (l *linkState) set(up bool, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.up && !up {
		l.since = time.Now()
	}
	l.up = up
	if reason != "" {
		l.reason = reason
	}
}

// setLink records the link state and tells ClientConfig.LinkChanged.
func (c *Client) setLink(up bool, reason string) {
	c.link.set(up, reason)
	if c.cfg.LinkChanged != nil {
		c.cfg.LinkChanged(up, reason)
	}
}

// logWhileOffline warns every offlineLogEvery, until ctx ends, while no session is up.
func (l *linkState) logWhileOffline(ctx context.Context) {
	tick := time.NewTicker(offlineLogEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		l.mu.Lock()
		up, since, reason := l.up, l.since, l.reason
		l.mu.Unlock()
		if !up {
			slog.WarnContext(ctx, "runner still offline, retrying", slog.Duration("offline_for", time.Since(since).Round(time.Second)), slog.String("last_error", reason))
		}
	}
}

func NewClient(cfg ClientConfig) (*Client, error) {
	if _, err := StreamURL(cfg.OrgURL, cfg.RunnerID); err != nil {
		return nil, err
	}
	if len(cfg.PrivateKey) != ed25519.PrivateKeySize {
		return nil, ErrAuth
	}
	h := cfg.Hooks
	if h.Call == nil || h.Open == nil || h.Clock == nil || h.Pending == nil || h.StatePages == nil || h.Replay == nil || h.BeforeReady == nil ||
		h.Lease == nil || h.Revoked == nil || h.Offline == nil {
		return nil, errors.New("runner execution and durable offline handlers must be configured before serving")
	}
	return &Client{cfg: cfg}, nil
}

// Run reconnects with bounded exponential jitter: initially 1–2s, capped at
// 60s. Revocation is terminal and erases through the durable offline handler.
func (c *Client) Run(ctx context.Context) error {
	attempt := 0
	var failures connectLog
	c.link.mu.Lock()
	c.link.up, c.link.since = false, time.Now()
	c.link.mu.Unlock()
	watch, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go c.link.logWhileOffline(watch)
	for ctx.Err() == nil {
		started := time.Now()
		err := c.Connect(ctx)
		if err != nil {
			c.setLink(false, err.Error())
		}
		if errors.Is(err, ErrRevoked) {
			return errors.Join(err, c.cfg.Hooks.Revoked(ctx))
		}
		if errors.Is(err, ErrGoaway) || errors.Is(err, ErrResumeGap) {
			c.session, c.ready = "", false
		}
		if c.ready && c.ended.After(started) {
			attempt = 0
			failures = connectLog{}
		}
		delay := reconnectDelay(attempt)
		if ctx.Err() == nil {
			failures.failed(ctx, err, delay)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if attempt < 6 {
			attempt++
		}
	}
	return ctx.Err()
}

// connectLog warns when the reason a connection failed differs from the last
// one, so a runner that keeps retrying says why it is offline without
// repeating itself. A session that reaches READY starts a new log.
type connectLog struct{ last string }

func (l *connectLog) failed(ctx context.Context, err error, retry time.Duration) {
	if err == nil || err.Error() == l.last {
		return
	}
	l.last = err.Error()
	attrs := []any{slog.Any("err", err), slog.Duration("retry_in", retry)}
	if hint := connectHint(err); hint != "" {
		attrs = append(attrs, slog.String("hint", hint))
	}
	slog.WarnContext(ctx, "runner connection to the organisation failed", attrs...)
}

func reconnectDelay(attempt int) time.Duration {
	base := time.Second << min(max(attempt, 0), 6)
	return min(base+time.Duration(rand.Int64N(int64(base))), 60*time.Second) //nolint:gosec // reconnect jitter is not a secret
}

// Connect serves a single connection; Run supplies the retry policy. It keeps
// only completed sessions eligible for resume and always resets byte streams.
func (c *Client) Connect(ctx context.Context) error {
	dial := c.cfg.Dial
	if dial == nil {
		dial = func(ctx context.Context) (Conn, error) {
			return DialWebSocket(ctx, c.cfg.OrgURL, c.cfg.RunnerID, c.cfg.HTTPClient)
		}
	}
	conn, err := dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	cfg, resume := c.resume(ctx)
	ok, err := authenticateOrg(ctx, conn, c.cfg.RunnerID, c.cfg.OrgURL, c.cfg.PrivateKey, resume, c.cfg.Hooks.Clock)
	if err != nil {
		return err
	}
	if !ok.Resumed {
		cfg = PeerConfig{}
	}
	if ok.ProxyImage != "" && c.cfg.Hooks.ProxyImage != nil {
		c.cfg.Hooks.ProxyImage(ok.ProxyImage)
	}
	if c.cfg.Hooks.LocalAttach != nil {
		c.cfg.Hooks.LocalAttach(ok.LocalAttach)
	}
	c.session, c.ready = ok.SessionID, false
	return c.serve(ctx, conn, cfg, ok)
}

func (c *Client) resume(ctx context.Context) (PeerConfig, *Resume) {
	if !c.ready || c.peer == nil || time.Since(c.ended) > ResumeWindow {
		return PeerConfig{}, nil
	}
	cfg, err := c.peer.ResumeConfig(ctx)
	if err != nil {
		return PeerConfig{}, nil
	}
	return cfg, &Resume{SessionID: c.session, LastSeq: cfg.LastReceived}
}

func (c *Client) serve(ctx context.Context, conn Conn, cfg PeerConfig, ok AuthOK) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pending := make(chan []PendingAction, 1)
	var ready atomic.Bool
	started := false
	var peer *Peer
	c.configure(ctx, &cfg, &ready, pending, func() *Peer { return peer })
	peer = NewPeer(conn, cfg)
	c.peer = peer
	defer func() {
		c.setLink(false, "")
		peer.Close()
		c.ended, c.ready = time.Now(), started && ready.Load()
		if c.cfg.Hooks.StreamOffline != nil {
			c.cfg.Hooks.StreamOffline(peer)
		}
		c.cfg.Hooks.Offline(ctx)
	}()
	if ok.Resumed {
		if err := c.cfg.Hooks.BeforeReady(ctx, peer, true); err != nil {
			return err
		}
		ready.Store(true)
	}
	if err := peer.Start(ctx, ok.LastSeq); err != nil {
		return err
	}
	started = true
	if !ok.Resumed {
		if err := c.fresh(ctx, peer, pending, &ready); err != nil {
			return err
		}
	}
	slog.InfoContext(ctx, "runner connected to the organisation", slog.Bool("resumed", ok.Resumed))
	c.setLink(true, "")
	if c.cfg.Hooks.Online != nil {
		c.cfg.Hooks.Online(peer, ok.Resumed)
	}
	select {
	case <-peer.Done():
		return peer.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) configure(ctx context.Context, cfg *PeerConfig, ready *atomic.Bool, pending chan<- []PendingAction, peer func() *Peer) {
	cfg.Org = false
	cfg.OnCall = func(ctx context.Context, id uint32, method string, args json.RawMessage) (any, error) {
		if !ready.Load() {
			return nil, ErrNotReady
		}
		return c.cfg.Hooks.Call(ctx, peer(), id, method, args)
	}
	cfg.OnOpen = func(s *Stream, o Open) error {
		if !ready.Load() {
			return ErrNotReady
		}
		return c.cfg.Hooks.Open(peer(), s, o)
	}
	receivedPending := false
	cfg.OnControlChecked = func(f Frame) error {
		switch f.Type {
		case TypePending:
			if ready.Load() || receivedPending {
				return fmt.Errorf("%w: unexpected PENDING", ErrBadFrame)
			}
			var actions []PendingAction
			if err := Unmarshal(f.Payload, &actions); err != nil {
				return err
			}
			receivedPending = true
			pending <- actions
			return nil
		case TypeLease:
			var lease Lease
			if !ready.Load() || Unmarshal(f.Payload, &lease) != nil {
				return fmt.Errorf("%w: unexpected LEASE", ErrBadFrame)
			}
			return c.cfg.Hooks.Lease(ctx, lease)
		case TypeRevoked:
			return ErrRevoked
		case TypeGoaway:
			return goaway(f.Payload)
		default:
			return fmt.Errorf("%w: unexpected %s", ErrBadFrame, f.Type)
		}
	}
}

func (c *Client) fresh(ctx context.Context, peer *Peer, pending <-chan []PendingAction, ready *atomic.Bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(peer.ctx, cancel)
	defer stop()
	select {
	case actions := <-pending:
		if err := c.cfg.Hooks.Pending(ctx, peer, actions); err != nil {
			return err
		}
	case <-peer.Done():
		return peer.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := c.sendInventory(ctx, peer); err != nil {
		return err
	}
	if err := c.cfg.Hooks.Replay(ctx, peer); err != nil {
		return err
	}
	if err := c.cfg.Hooks.BeforeReady(ctx, peer, false); err != nil {
		return err
	}
	ready.Store(true)
	if err := peer.Send(ctx, Frame{Type: TypeReady}); err != nil {
		ready.Store(false)
		return err
	}
	return nil
}

// sendInventory refuses to reach READY unless the hook emitted a valid Final
// and no page was refused, even if the hook discarded that refusal.
func (c *Client) sendInventory(ctx context.Context, peer *Peer) error {
	var cursor StateInventory
	var refused error
	err := c.cfg.Hooks.StatePages(ctx, func(page StatePage) error {
		if refused != nil {
			return refused
		}
		next, err := cursor.Next(page)
		if err == nil {
			var payload []byte
			if payload, err = Marshal(page); err == nil {
				err = peer.Send(ctx, Frame{Type: TypeState, Payload: payload})
			}
		}
		if err != nil {
			refused = err
			return err
		}
		cursor = next
		return nil
	})
	if err == nil {
		err = refused
	}
	if err == nil && !cursor.Complete {
		err = fmt.Errorf("%w: STATE inventory ended without Final", ErrBadFrame)
	}
	return err
}
