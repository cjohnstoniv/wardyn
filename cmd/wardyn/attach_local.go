// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Local attach (design ATTACH §2.2, §3.5): when the run sits on a runner of this computer, `wardyn
// attach` keeps the terminal here, on that runner's socket, and the organisation's WebSocket carries
// only control: the holder registration, the mode frames, presence and the close reason. The
// organisation still admits the session (the same mint and the same WebSocket admission as the relay)
// and records it. Any refusal of either is shown as the server gives it and never retried another way.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// The --via values.
const (
	viaAuto  = "auto"
	viaLocal = "local"
	viaRelay = "relay"
)

// localAttachWindowsRefusal is --via local on Windows, where the runner lives in WSL2.
const localAttachWindowsRefusal = "local attach is not available on Windows: the runner lives in WSL2; use --via relay"

// attachOptions are `wardyn attach`'s flags.
type attachOptions struct {
	via      string // auto (also ""), local or relay
	takeOver bool
}

// planVia reads --via for an OS that has (localOS) or lacks the local transport: whether to try local,
// and whether a local try that cannot be made is final (--via local) rather than a reason to relay.
func planVia(via string, localOS bool) (try, must bool, err error) {
	switch via {
	case "", viaAuto:
		return localOS, false, nil
	case viaRelay:
		return false, false, nil
	case viaLocal:
		if !localOS {
			return false, false, errors.New(localAttachWindowsRefusal)
		}
		return true, true, nil
	}
	return false, false, fmt.Errorf("--via must be auto, local or relay, not %q", via)
}

// The local socket's frames, as runnerd speaks them (internal/runner/runnerio/local_attach.go on the
// runner's side): one type byte, a big-endian uint32 payload length, then the payload, JSON for control
// frames and raw terminal bytes for localData.
const (
	localHello   byte = 'h' // CLI, first frame: {v, run_id}
	localHelloOK byte = 'o' // runnerd: {runner_id, nonce}
	localRefuse  byte = 'x' // runnerd, before localBound: {reason}, then it closes
	localBound   byte = 'b' // runnerd: the organisation's grant arrived
	localData    byte = 'd' // either way: terminal bytes
	localResize  byte = 'r' // CLI: {cols, rows}
	localMode    byte = 'm' // runnerd: {read_only}
	localEnd     byte = 'e' // runnerd, after localBound: {reason}, then it closes

	localProtocol = 1
	maxLocalFrame = 64 << 10
)

// localAttachDir is where runner pointer files live; a seam for tests.
var localAttachDir = defaultLocalAttachDir

// presenceEvery bounds the presence frames a local session sends while the person types.
var presenceEvery = 10 * time.Second

// localEndGrace is how long a session the runner ended waits for the organisation's close, whose reason
// is the one shown.
var localEndGrace = 2 * time.Second

// localHelloTimeout bounds the socket's answer to the hello.
const localHelloTimeout = 10 * time.Second

func writeLocalFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > maxLocalFrame {
		return fmt.Errorf("local frame of %d bytes is over %d", len(payload), maxLocalFrame)
	}
	frame := make([]byte, 5, 5+len(payload))
	frame[0] = typ
	binary.BigEndian.PutUint32(frame[1:], uint32(len(payload)))
	_, err := w.Write(append(frame, payload...))
	return err
}

func readLocalFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxLocalFrame {
		return 0, nil, fmt.Errorf("local frame of %d bytes is over %d", n, maxLocalFrame)
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return hdr[0], b, err
}

// localConn is one connection to the runner's socket that holds a nonce.
type localConn struct {
	conn   net.Conn
	nonce  string
	runner string // the runner's name, for the transport line
	wmu    sync.Mutex
}

func (l *localConn) send(typ byte, payload []byte) error {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	return writeLocalFrame(l.conn, typ, payload)
}

// openLocal is discovery and the hello (§3.5.1, steps 2-3 of §3.5.3): the run must be on a runner whose
// pointer file is on this computer, this user's, naming this organisation, and that runner must hand
// out a nonce. nil says why not; the caller relays, or under --via local stops there.
func openLocal(ctx context.Context, c *sdk.Client, runID string) (*localConn, string) {
	id, err := uuid.Parse(runID)
	if err != nil {
		return nil, "the run id is not a UUID"
	}
	run, err := c.GetRun(ctx, id)
	if err != nil {
		return nil, "the run could not be read: " + err.Error()
	}
	if run.Placement != types.PlacementLocal || run.RunnerID == nil {
		return nil, "the run is not on a person's runner"
	}
	dir, err := localAttachDir()
	if err != nil {
		return nil, "no local attach directory: " + err.Error()
	}
	sock, why := localSocketFor(dir, run.RunnerID.String(), c.BaseURL)
	if sock == "" {
		return nil, why
	}
	d := net.Dialer{Timeout: localHelloTimeout}
	conn, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, "the runner's socket did not answer: " + err.Error()
	}
	hello, _ := json.Marshal(map[string]any{"v": localProtocol, "run_id": id})
	_ = conn.SetDeadline(time.Now().Add(localHelloTimeout))
	typ, b, err := func() (byte, []byte, error) {
		if err := writeLocalFrame(conn, localHello, hello); err != nil {
			return 0, nil, err
		}
		return readLocalFrame(conn)
	}()
	_ = conn.SetDeadline(time.Time{})
	var ok struct {
		RunnerID string `json:"runner_id"`
		Nonce    string `json:"nonce"`
		Reason   string `json:"reason"`
	}
	switch {
	case err != nil:
		why = "the runner's socket closed: " + err.Error()
	case json.Unmarshal(b, &ok) != nil:
		why = "the runner's socket sent an unreadable frame"
	case typ == localRefuse:
		why = "the runner refused: " + ok.Reason
	case typ != localHelloOK || ok.Nonce == "" || ok.RunnerID != run.RunnerID.String():
		why = "the runner's socket answered for another runner"
	default:
		name := run.RunnerName
		if name == "" {
			name = ok.RunnerID
		}
		return &localConn{conn: conn, nonce: ok.Nonce, runner: name}, ""
	}
	_ = conn.Close()
	return nil, why
}

// localUnavailable is the organisation's pre-upgrade transport answer (409 attach_local_unavailable):
// not a refusal, so the CLI attaches through the organisation instead.
func localUnavailable(err error) bool {
	var ae *sdk.APIError
	return errors.As(err, &ae) && ae.Status == 409 && ae.Reason == "attach_local_unavailable"
}

// localGrantFailed is close 4409: the runner did not take the organisation's grant. The CLI retries it
// once through the organisation, with a new ticket.
func localGrantFailed(err error) bool {
	return websocket.CloseStatus(err) == 4409
}

// withLocalTransport asks the attach WebSocket for the local transport with the runner's nonce.
func withLocalTransport(wsURL, nonce string) string {
	u, err := url.Parse(wsURL)
	if err != nil {
		return wsURL
	}
	q := u.Query()
	q.Set("transport", "local")
	q.Set("local_nonce", nonce)
	u.RawQuery = q.Encode()
	return u.String()
}

// isLocalModeFrame reports whether a first WebSocket frame says the organisation runs this socket as a
// local session. An older organisation ignores transport=local and sends no "transport": that socket is
// an ordinary relay attach.
func isLocalModeFrame(typ websocket.MessageType, data []byte) bool {
	var m struct {
		Type      string `json:"type"`
		Transport string `json:"transport"`
	}
	return typ == websocket.MessageText && json.Unmarshal(data, &m) == nil && m.Type == "attach-mode" && m.Transport == "local"
}

// The transport line: one stderr line naming the path the session took.
const attachLineTail = ". Ctrl-C goes to the sandbox; close the window to detach."

func localAttachLine(runner string) string {
	return "Attached on this computer (runner " + runner + ")" + attachLineTail
}

func relayAttachLine(baseURL string) string {
	host := baseURL
	if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
		host = u.Host
	}
	return "Attached through the organisation (" + host + ")" + attachLineTail
}

// attachPipe is one session's transport, as the pump drives it.
type attachPipe struct {
	// output runs until the session ends: show gets terminal bytes, mode the organisation's mode frames.
	output func(ctx context.Context, show func([]byte) error, mode func([]byte)) error
	input  func(ctx context.Context, p []byte) error
	resize func(ctx context.Context, cols, rows uint16) error
	close  func()
}

// relayPipe carries the terminal on the WebSocket. pending is a frame the transport probe already read.
func relayPipe(ws *websocket.Conn, pending *wsFrame) attachPipe {
	return attachPipe{
		output: func(ctx context.Context, show func([]byte) error, mode func([]byte)) error {
			for {
				var f wsFrame
				if pending != nil {
					f, pending = *pending, nil
				} else {
					var err error
					if f.typ, f.data, err = ws.Read(ctx); err != nil {
						return err
					}
				}
				switch f.typ {
				case websocket.MessageBinary:
					if err := show(f.data); err != nil {
						return err
					}
				case websocket.MessageText:
					// The only TEXT frame the server sends is attach-mode (attach_holder.go's
					// attachModeMsg). stdout gets none of it.
					mode(f.data)
				}
			}
		},
		input: func(ctx context.Context, p []byte) error { return ws.Write(ctx, websocket.MessageBinary, p) },
		resize: func(ctx context.Context, cols, rows uint16) error {
			return sendResize(ctx, ws, cols, rows)
		},
		close: func() { _ = ws.Close(websocket.StatusNormalClosure, "") },
	}
}

type wsFrame struct {
	typ  websocket.MessageType
	data []byte
}

// The runner's end reasons that are a clean detach, as the relay's normal close is.
var localCleanEnds = map[string]bool{"shell_exited": true, "run_ended": true}

// localPipe carries the terminal on the runner's socket; the WebSocket stays open beside it for control.
// first is the organisation's first attach-mode frame, which the transport probe read.
func localPipe(ws *websocket.Conn, lc *localConn, first []byte) attachPipe {
	var lastPresence time.Time // input's alone
	return attachPipe{
		output: func(ctx context.Context, show func([]byte) error, mode func([]byte)) error {
			mode(first)
			wsDone := make(chan error, 1)
			go func() {
				for {
					typ, data, err := ws.Read(ctx)
					if err != nil {
						wsDone <- err
						return
					}
					if typ == websocket.MessageText {
						mode(data)
					}
				}
			}()
			localDone := make(chan error, 1)
			go func() { localDone <- localOutput(lc, show) }()
			select {
			case err := <-wsDone:
				return err
			case <-ctx.Done():
				return ctx.Err()
			case lerr := <-localDone:
				// The runner ended the terminal. The organisation's close names why (take-over,
				// sign-out, lease lost) as on the relay, so wait for it before saying anything.
				t := time.NewTimer(localEndGrace)
				defer t.Stop()
				select {
				case err := <-wsDone:
					if !isNormalClose(err) {
						return err
					}
				case <-ctx.Done():
					return ctx.Err()
				case <-t.C:
				}
				var end *localEnded
				if errors.As(lerr, &end) && localCleanEnds[end.reason] {
					return nil
				}
				return lerr
			}
		},
		input: func(ctx context.Context, p []byte) error {
			for len(p) > 0 {
				n := min(len(p), maxLocalFrame)
				if err := lc.send(localData, p[:n]); err != nil {
					return err
				}
				p = p[n:]
			}
			// Presence: the person is typing. At most one frame per presenceEvery.
			if now := time.Now(); now.Sub(lastPresence) >= presenceEvery {
				lastPresence = now
				return ws.Write(ctx, websocket.MessageText, []byte(`{"type":"presence"}`))
			}
			return nil
		},
		resize: func(ctx context.Context, cols, rows uint16) error {
			b, _ := json.Marshal(map[string]uint16{"cols": cols, "rows": rows})
			if err := lc.send(localResize, b); err != nil {
				return err
			}
			// The organisation records the window size for the cast.
			return sendResize(ctx, ws, cols, rows)
		},
		close: func() {
			_ = lc.conn.Close()
			_ = ws.Close(websocket.StatusNormalClosure, "")
		},
	}
}

// localEnded is the runner's end of a local session, with its reason.
type localEnded struct{ reason string }

func (e *localEnded) Error() string { return "the local session ended: " + e.reason }

// localOutput shows the runner's terminal bytes until it ends the session.
func localOutput(lc *localConn, show func([]byte) error) error {
	for {
		typ, b, err := readLocalFrame(lc.conn)
		if err != nil {
			return &localEnded{reason: "the runner's socket closed"}
		}
		switch typ {
		case localData:
			if err := show(b); err != nil {
				return err
			}
		case localEnd, localRefuse:
			var m struct {
				Reason string `json:"reason"`
			}
			_ = json.Unmarshal(b, &m)
			return &localEnded{reason: m.Reason}
		}
		// localBound and localMode need nothing here: the organisation's attach-mode frames carry the
		// same change, and those print the notices.
	}
}
