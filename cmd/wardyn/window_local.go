// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// A window onto a Linux VM run: the runner on this computer hands its local socket a window nonce, the
// organisation grants the window for it, and the runner then carries the VM's display over that socket to
// the CLI's built-in viewer. No display byte crosses the organisation.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/localviewer"
	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// localKindWindow is the hello's kind for a window nonce; an empty kind asks for a terminal.
const localKindWindow = "window"

// localWindowOSSentence is `wardyn window` on a computer that cannot draw the window.
const localWindowOSSentence = "a window onto a Linux VM run needs a Linux computer; open the run's desktop in the console instead"

// The viewer, as seams for tests.
var (
	localViewerRun       = localviewer.Run
	localViewerAvailable = localviewer.Available
)

// localWindowEnds words the reasons the runner ends a window with. ok marks the ends that are not a
// refusal: the run's display closing.
var localWindowEnds = map[string]struct {
	sentence string
	ok       bool
}{
	"client_closed":         {"The window closed. The run goes on.", true},
	"run_ended":             {"The run's display closed, so the window did too.", true},
	"ended_by_organisation": {"The organisation closed the window: the run may have paused or stopped, or windows were turned off.", false},
	"runner_link_lost":      {"The runner lost its link to the organisation, so the window closed.", false},
	"display_refused":       {"The runner refused the run's display stream, so the window closed.", false},
	"nonce_kind_mismatch":   {"The runner was asked for a terminal on a window's nonce and refused.", false},
	"protocol":              {"The window broke the runner's local protocol and was closed.", false},
	"attach_failed":         {"The runner could not reach the run's display.", false},
}

// localWindowEnd is the error of a window the runner ended with a refusal; it prints as the sentence.
type localWindowEnd struct{ sentence string }

func (e *localWindowEnd) Error() string { return e.sentence }

// runLocalWindow opens run's display in a window here: the window hello on the runner's socket, the
// organisation's grant for its nonce, then the viewer until the window ends.
func runLocalWindow(ctx context.Context, c *sdk.Client, run types.AgentRun, out io.Writer) error {
	if err := localViewerAvailable(); err != nil {
		if errors.Is(err, localviewer.ErrUnsupportedOS) {
			return errors.New(localWindowOSSentence)
		}
		return err
	}
	lc, why := openLocalWindowSocket(ctx, c, run)
	if lc == nil {
		return localDiscoveryFailed(ctx, c, run, why)
	}
	defer lc.conn.Close()
	if _, err := c.OpenLocalWindow(ctx, run.ID, lc.nonce); err != nil {
		return sentenceError(err)
	}
	// The runner says LocalBound before the organisation answers the grant.
	_ = lc.conn.SetReadDeadline(time.Now().Add(localHelloTimeout))
	typ, b, err := readLocalFrame(lc.conn)
	_ = lc.conn.SetReadDeadline(time.Time{})
	if err != nil {
		return errors.New("the runner's socket closed before the window opened: " + err.Error())
	}
	if typ == localRefuse || typ == localEnd {
		return endError(reasonOf(b))
	}
	if typ != localBound {
		return errors.New("the runner's socket sent an unexpected frame before the window opened")
	}
	fmt.Fprintf(out, "The window is open on %s. Closing it ends the window; the run goes on.\n", lc.runner)
	title := run.Title
	if title == "" {
		title = run.ID.String()
	}
	pipe := &localWindowPipe{lc: lc}
	verr := localViewerRun(ctx, pipe, "Wardyn: "+title)
	if reason := pipe.endReason(); reason != "" {
		if err := endError(reason); err != nil {
			return err
		}
		fmt.Fprintln(out, localWindowEnds[reason].sentence)
		return nil
	}
	if verr != nil {
		return fmt.Errorf("the window failed: %w", verr)
	}
	fmt.Fprintln(out, localWindowEnds["client_closed"].sentence)
	return nil
}

// localDiscoveryFailed asks the organisation to open the window with no nonce when this computer's runner
// handed none out, so a refusal the organisation would give anyway (the feature turned off, not your
// computer, the runner offline ...) is shown in its own words. The organisation checks the nonce only after
// those, and before it reserves the window or calls the runner, so only its missing-nonce answer leaves the
// local reason to say.
func localDiscoveryFailed(ctx context.Context, c *sdk.Client, run types.AgentRun, why string) error {
	local := errors.New("a window onto a Linux VM run opens only on the computer its runner is on, and that runner " +
		"handed out no window nonce here (" + why + "); a runner picks up a change to the window setting when it next connects")
	_, err := c.OpenLocalWindow(ctx, run.ID, "")
	var ae *sdk.APIError
	if err == nil || (errors.As(err, &ae) && ae.Status == http.StatusBadRequest && ae.Reason == localNonceMissingReason) {
		return local
	}
	return sentenceError(err)
}

// localNonceMissingReason is the organisation's answer to an open with no nonce.
const localNonceMissingReason = "invalid_request_body"

// endError is nil for an end that is not a refusal, and otherwise the end's sentence as an error.
func endError(reason string) error {
	e, known := localWindowEnds[reason]
	switch {
	case known && e.ok:
		return nil
	case known:
		return &localWindowEnd{sentence: e.sentence}
	}
	return &localWindowEnd{sentence: "The runner closed the window: " + reason + "."}
}

func reasonOf(b []byte) string {
	var m struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(b, &m) != nil || m.Reason == "" {
		return "protocol"
	}
	return m.Reason
}

// openLocalWindowSocket is openLocal's discovery with the window hello: the run must be on a runner whose
// pointer file is on this computer, this user's, naming this organisation, and that runner must hand out
// a window nonce. nil says why not.
func openLocalWindowSocket(ctx context.Context, c *sdk.Client, run types.AgentRun) (*localConn, string) {
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
	hello, _ := json.Marshal(map[string]any{"v": localProtocol, "run_id": run.ID, "kind": localKindWindow})
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

// localWindowPipe is the viewer's RFB stream over the runner's socket: it reads localData frames' bytes
// and writes its own as localData frames, and a localEnd frame ends it with the runner's reason.
type localWindowPipe struct {
	lc      *localConn
	pending []byte
	mu      sync.Mutex
	end     string // guarded by mu
}

func (p *localWindowPipe) Read(b []byte) (int, error) {
	for len(p.pending) == 0 {
		typ, payload, err := readLocalFrame(p.lc.conn)
		if err != nil {
			return 0, err
		}
		switch typ {
		case localData:
			p.pending = payload
		case localEnd:
			p.mu.Lock()
			p.end = reasonOf(payload)
			p.mu.Unlock()
			return 0, io.EOF
		default:
			return 0, fmt.Errorf("the runner's socket sent frame %q during a window", typ)
		}
	}
	n := copy(b, p.pending)
	p.pending = p.pending[n:]
	return n, nil
}

func (p *localWindowPipe) Write(b []byte) (int, error) {
	for off := 0; off < len(b); off += maxLocalFrame {
		if err := p.lc.send(localData, b[off:min(off+maxLocalFrame, len(b))]); err != nil {
			return off, err
		}
	}
	return len(b), nil
}

// Close closes the socket, which the runner reads as the person closing the window.
func (p *localWindowPipe) Close() error { return p.lc.conn.Close() }

func (p *localWindowPipe) endReason() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.end
}
