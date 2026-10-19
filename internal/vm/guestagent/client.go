// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
)

// Client is runnerd's side of one guest agent. It speaks HTTP/2 over the connections dial makes (the
// QEMU chardev accepts one at a time, so the transport keeps to one).
type Client struct {
	hc *http.Client
	tr *http.Transport

	mu    sync.Mutex
	conns []net.Conn
}

// NewClient returns a client that reaches the agent through dial.
func NewClient(dial func(ctx context.Context) (net.Conn, error)) *Client {
	c := &Client{}
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	c.tr = &http.Transport{
		Protocols:       &protocols,
		MaxConnsPerHost: 1,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			conn, err := dial(ctx)
			if err != nil {
				return nil, err
			}
			c.mu.Lock()
			c.conns = append(c.conns, conn)
			c.mu.Unlock()
			return conn, nil
		},
	}
	c.hc = &http.Client{Transport: c.tr}
	return c
}

// Close closes every connection the client made, in use or not.
func (c *Client) Close() {
	c.tr.CloseIdleConnections()
	c.mu.Lock()
	conns := c.conns
	c.conns = nil
	c.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

const base = "http://guest"

// Ready asks the agent for its version: the first call after boot.
func (c *Client) Ready(ctx context.Context) (Ready, error) {
	var r Ready
	return r, c.do(ctx, http.MethodGet, "/v1/ready", nil, &r)
}

// Init hands the agent the sandbox's env, files and mounts; the agent refuses a second one.
func (c *Client) Init(ctx context.Context, in Init) error {
	return c.do(ctx, http.MethodPost, "/v1/init", in, nil)
}

// State is a detached exec's state.
func (c *Client) State(ctx context.Context, id string) (ExecState, error) {
	var s ExecState
	return s, c.do(ctx, http.MethodGet, "/v1/exec/"+url.PathEscape(id), nil, &s)
}

// Agent is the latest detached exec's state; ErrNoExec when none was started this boot.
func (c *Client) Agent(ctx context.Context) (ExecState, error) {
	var s ExecState
	return s, c.do(ctx, http.MethodGet, "/v1/agent", nil, &s)
}

// ErrNoExec is the agent's answer for an exec id it does not hold.
var ErrNoExec = errors.New("guestagent: no such exec")

// Output streams a detached exec's output from offset, until it has exited and all of it was read.
func (c *Client) Output(ctx context.Context, id string, offset int64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/exec/"+url.PathEscape(id)+"/output?offset="+strconv.FormatInt(offset, 10), nil)
	if err != nil {
		return nil, err
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if err := statusErr(res); err != nil {
		return nil, err
	}
	return res.Body, nil
}

// Start starts a detached exec and returns its id.
func (c *Client) Start(ctx context.Context, er ExecRequest) (string, error) {
	er.Detach = true
	spec, err := json.Marshal(er)
	if err != nil {
		return "", err
	}
	var body bytes.Buffer
	if err := WriteFrame(&body, FrameSpec, spec); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/exec", &body)
	if err != nil {
		return "", err
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if err := statusErr(res); err != nil {
		return "", err
	}
	typ, payload, err := ReadFrame(res.Body)
	switch {
	case err != nil:
		return "", err
	case typ == FrameError:
		return "", fmt.Errorf("guestagent: start: %s", payload)
	case typ != FrameStarted || len(payload) == 0:
		return "", fmt.Errorf("guestagent: start answered frame 0x%02x", typ)
	}
	return string(payload), nil
}

// Session is one attached exec.
type Session struct {
	Stdin  io.WriteCloser // closing it sends end of input
	Stdout io.Reader
	Stderr io.Reader // empty for a TTY exec
	cancel context.CancelFunc
	up     *io.PipeWriter
	upMu   sync.Mutex
	done   chan struct{}
	code   int
	err    error
}

// Exec starts an attached exec. The session lives until it exits or Close.
func (c *Client) Exec(ctx context.Context, er ExecRequest) (*Session, error) {
	er.Detach = false
	spec, err := json.Marshal(er)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	upR, upW := io.Pipe()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/exec", upR)
	if err != nil {
		cancel()
		return nil, err
	}
	s := &Session{cancel: cancel, up: upW, done: make(chan struct{})}
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	s.Stdout, s.Stderr = outR, errR
	s.Stdin = stdinWriter{s}
	// The spec frame is written while the request is in flight: the pipe has no buffer.
	go func() {
		if err := s.send(FrameSpec, spec); err != nil {
			upW.CloseWithError(err)
		}
	}()
	res, err := c.hc.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if err := statusErr(res); err != nil {
		cancel()
		return nil, err
	}
	go func() {
		defer close(s.done)
		defer res.Body.Close()
		s.code, s.err = demux(res.Body, outW, errW)
		outW.CloseWithError(s.err)
		errW.CloseWithError(s.err)
		_ = upW.Close()
	}()
	return s, nil
}

// demux copies stdout and stderr frames to their writers until the exit frame.
func demux(r io.Reader, stdout, stderr *io.PipeWriter) (int, error) {
	for {
		typ, payload, err := ReadFrame(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return -1, fmt.Errorf("guestagent: exec ended without an exit code: %w", err)
		}
		switch typ {
		case FrameStdout:
			_, _ = stdout.Write(payload) // a reader that went away drops output, as a closed pty does
		case FrameStderr:
			_, _ = stderr.Write(payload)
		case FrameExit:
			return readInt32(payload)
		case FrameError:
			return -1, fmt.Errorf("guestagent: exec: %s", payload)
		default:
			return -1, fmt.Errorf("guestagent: unexpected frame 0x%02x", typ)
		}
	}
}

func (s *Session) send(typ byte, payload []byte) error {
	s.upMu.Lock()
	defer s.upMu.Unlock()
	return WriteFrame(s.up, typ, payload)
}

type stdinWriter struct{ s *Session }

func (w stdinWriter) Write(p []byte) (int, error) {
	for off := 0; off < len(p); off += MaxFrame {
		if err := w.s.send(FrameStdin, p[off:min(off+MaxFrame, len(p))]); err != nil {
			return off, err
		}
	}
	return len(p), nil
}

func (w stdinWriter) Close() error { return w.s.send(FrameStdinClose, nil) }

// Resize sets the session's terminal size; a no-op on the guest for a non-TTY exec.
func (s *Session) Resize(cols, rows uint16) error { return s.send(FrameResize, uint16Pair(cols, rows)) }

// Wait blocks until the exec exits and returns its code.
func (s *Session) Wait() (int, error) {
	<-s.done
	return s.code, s.err
}

// Close ends the session; the guest kills the process when its stream is cancelled.
func (s *Session) Close() error {
	s.cancel()
	_ = s.up.Close()
	<-s.done
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return err
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if err := statusErr(res); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(res.Body, MaxFrame)).Decode(out)
}

func statusErr(res *http.Response) error {
	if res.StatusCode == http.StatusOK {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
	res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", ErrNoExec, bytes.TrimSpace(msg))
	}
	return fmt.Errorf("guestagent: %s: %s", res.Status, bytes.TrimSpace(msg))
}
