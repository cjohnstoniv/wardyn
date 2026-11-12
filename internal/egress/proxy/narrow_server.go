// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// liveState is what a live egress edit acts on: the client connections the listeners hold, which it
// drops, and the loaded-report loop, which it replaces.
type liveState struct {
	mu    sync.Mutex
	conns map[*trackedConn]struct{}

	reportMu     sync.Mutex
	reportCancel context.CancelFunc
}

// closeAll closes every client connection the listeners hold.
func (l *liveState) closeAll() {
	l.mu.Lock()
	conns := make([]*trackedConn, 0, len(l.conns))
	for c := range l.conns {
		conns = append(conns, c)
	}
	l.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// trackedListener records each connection it accepts in set until that connection is closed.
type trackedListener struct {
	net.Listener
	set *liveState
}

func (l *trackedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	t := &trackedConn{Conn: c, set: l.set}
	l.set.mu.Lock()
	if l.set.conns == nil {
		l.set.conns = make(map[*trackedConn]struct{})
	}
	l.set.conns[t] = struct{}{}
	l.set.mu.Unlock()
	return t, nil
}

// trackedConn removes itself from its set when closed.
type trackedConn struct {
	net.Conn
	set *liveState
}

func (c *trackedConn) Close() error {
	c.set.mu.Lock()
	delete(c.set.conns, c)
	c.set.mu.Unlock()
	return c.Conn.Close()
}

// CloseWrite keeps tunnel's half-close reaching the wrapped connection.
func (c *trackedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// StartBootReport starts ReportLoaded for the config the proxy booted with, unless a narrowing already
// started its own report. A narrowing's config holds the boot list and more, so the boot report is
// never the newer one, and the narrowing socket may take an edit before the boot report starts.
func (s *Server) StartBootReport(ctx context.Context, r LoadedReport) {
	s.live.reportMu.Lock()
	defer s.live.reportMu.Unlock()
	if s.live.reportCancel != nil {
		return
	}
	s.startReportLocked(ctx, r)
}

// StartLoadedReport starts ReportLoaded for r after cancelling the loop it started before, so a report
// still retrying can no longer land after a newer one.
func (s *Server) StartLoadedReport(ctx context.Context, r LoadedReport) {
	s.live.reportMu.Lock()
	defer s.live.reportMu.Unlock()
	s.startReportLocked(ctx, r)
}

// startReportLocked is StartLoadedReport with s.live.reportMu held.
func (s *Server) startReportLocked(ctx context.Context, r LoadedReport) {
	if s.live.reportCancel != nil {
		s.live.reportCancel()
	}
	child, cancel := context.WithCancel(ctx)
	s.live.reportCancel = cancel
	go s.ReportLoaded(child, r)
}

// NarrowAnswer is the one JSON line the narrowing socket answers.
type NarrowAnswer struct {
	State        string   `json:"state"` // "current", "applied" or "refused"
	Changed      bool     `json:"changed"`
	ConfigSHA256 string   `json:"config_sha256,omitempty"`
	BaseSHA256   string   `json:"base_sha256,omitempty"`
	LiveDenies   []string `json:"live_denies"`
	Error        string   `json:"error,omitempty"`
}

// narrowRequest is the one JSON object the narrowing socket reads.
type narrowRequest struct {
	Deny *[]string `json:"deny"`
}

// narrowReadTimeout bounds how long one connection may take to send its request.
const narrowReadTimeout = 5 * time.Second

// fsyncDir flushes the directory holding the list, so the rename survives a crash. A variable, so a test can fail it.
var fsyncDir = func(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// narrowing is the state ServeNarrow's handler keeps between requests.
type narrowing struct {
	s      *Server
	ctx    context.Context
	dir    string
	base   []byte
	baseSH string
	report func([]byte) LoadedReport

	mu  sync.Mutex // one request at a time
	set []string   // the list the proxy enforces
	eff []byte     // the config bytes for set
}

// ServeNarrow listens on narrow.sock in dir and takes live deny requests there until ctx ends. base is
// the config bytes before any live deny list, and report builds the loaded report for bytes of config.
// Hosts a request names are added to the list kept in dir and enforced at once, and never removed.
func (s *Server) ServeNarrow(ctx context.Context, dir string, base []byte, report func([]byte) LoadedReport) error {
	baseSH, err := LiveDenyBaseSHA256(base)
	if err != nil {
		return err
	}
	set, err := ReadLiveDenyFile(dir)
	if err != nil {
		return err
	}
	eff, err := WithLiveDenies(base, set)
	if err != nil {
		return err
	}
	if set, err = liveDenyList(eff); err != nil {
		return err
	}
	path := NarrowSocketPath(dir)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return err
	}
	n := &narrowing{s: s, ctx: ctx, dir: dir, base: base, baseSH: baseSH, report: report, set: set}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				if ctx.Err() == nil {
					slog.Error("wardyn-proxy: the narrowing socket stopped", slog.Any("err", err))
				}
				return
			}
			go n.serveConn(c)
		}
	}()
	return nil
}

// ReadLiveDenyFile reads and checks the list kept in dir. A missing file is an empty list.
func ReadLiveDenyFile(dir string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(dir, liveDenyFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", liveDenyFile, err)
	}
	return NormalizeLiveDenies(list)
}

// NarrowSocketPath is where the proxy that keeps its live deny list in dir listens for new hosts.
func NarrowSocketPath(dir string) string { return filepath.Join(dir, narrowSocket) }

func (n *narrowing) serveConn(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(narrowReadTimeout))
	req, err := readNarrowRequest(c)
	n.mu.Lock()
	defer n.mu.Unlock()
	var ans *NarrowAnswer
	if err != nil {
		ans = n.refused(err)
	} else {
		ans = n.handle(req)
	}
	if ans == nil {
		return // no answer: the client sees the connection close
	}
	line, merr := json.Marshal(ans)
	if merr != nil {
		slog.Error("wardyn-proxy: encode the narrowing answer", slog.Any("err", merr))
		return
	}
	_ = c.SetWriteDeadline(time.Now().Add(narrowReadTimeout))
	_, _ = c.Write(append(line, '\n'))
}

// readNarrowRequest reads one {"deny": [...]} object of at most MaxNarrowMessageBytes. The request ends
// where the sender half-closes.
func readNarrowRequest(r io.Reader) ([]string, error) {
	body, err := io.ReadAll(io.LimitReader(r, MaxNarrowMessageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read the request: %w", err)
	}
	if len(body) > MaxNarrowMessageBytes {
		return nil, fmt.Errorf("request is over %d bytes", MaxNarrowMessageBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var req narrowRequest
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("request is not {\"deny\": [...]}: %w", err)
	}
	if req.Deny == nil {
		return nil, errors.New(`request has no "deny" list`)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("request has data after its object")
	}
	return *req.Deny, nil
}

// refused answers that nothing changed.
func (n *narrowing) refused(err error) *NarrowAnswer {
	return &NarrowAnswer{State: "refused", LiveDenies: append([]string{}, n.set...), Error: err.Error()}
}

// handle takes one request, with n.mu held. It answers nil when the request's outcome is uncertain.
func (n *narrowing) handle(deny []string) *NarrowAnswer {
	req, err := NormalizeLiveDenies(deny)
	if err != nil {
		return n.refused(err)
	}
	if len(req) == 0 {
		ans, err := n.answer("current", false)
		if err != nil {
			return n.refused(err)
		}
		return ans
	}
	next, err := NormalizeLiveDenies(append(append([]string{}, n.set...), req...))
	if err != nil {
		return n.refused(err)
	}
	changed := !slices.Equal(next, n.set)
	renamed, err := persistLiveDenies(n.dir, next)
	if err != nil && !renamed {
		return n.refused(fmt.Errorf("persist the live deny list: %w", err))
	}
	n.set = next
	if changed {
		n.s.proxy.policy.live.set(next)
		n.s.live.closeAll()
	}
	if err != nil {
		slog.Error("wardyn-proxy: sync the live deny directory; the request is left unanswered", slog.Any("err", err))
		return nil
	}
	ans, err := n.answer("applied", changed)
	if err != nil {
		// The list is kept and enforced, so a refusal would be false: the outcome is left uncertain.
		slog.Error("wardyn-proxy: build the narrowed config; the request is left unanswered", slog.Any("err", err))
		return nil
	}
	n.s.StartLoadedReport(n.ctx, n.report(n.eff))
	return ans
}

// withLiveDenies builds answer's config bytes. A variable, so a test can fail it.
var withLiveDenies = WithLiveDenies

// answer is the state's answer for the list n.set, and keeps the config bytes it hashed in n.eff.
func (n *narrowing) answer(state string, changed bool) (*NarrowAnswer, error) {
	eff, err := withLiveDenies(n.base, n.set)
	if err != nil {
		return nil, err
	}
	n.eff = eff
	return &NarrowAnswer{
		State: state, Changed: changed, ConfigSHA256: ConfigSHA256(eff), BaseSHA256: n.baseSH,
		LiveDenies: append([]string{}, n.set...),
	}, nil
}

// persistLiveDenies writes list to dir as a tmp file, fsyncs it, renames it over the list file and
// fsyncs dir. renamed reports whether the rename had returned when an error came.
func persistLiveDenies(dir string, list []string) (renamed bool, err error) {
	body, err := json.Marshal(list)
	if err != nil {
		return false, err
	}
	final := filepath.Join(dir, liveDenyFile)
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return false, err
	}
	fail := func(err error) (bool, error) {
		_ = f.Close()
		_ = os.Remove(tmp)
		return false, err
	}
	if err := f.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := f.Write(body); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	return true, fsyncDir(dir)
}
