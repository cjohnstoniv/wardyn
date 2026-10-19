// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package qemu

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"strings"
	"time"
)

// QMP is one QMP session (QEMU's machine protocol) on an open connection. Not safe for concurrent use.
type QMP struct {
	conn net.Conn
	r    *bufio.Reader
}

// reply is a QMP or QGA answer line; event lines (QMP only) are skipped.
type reply struct {
	Return json.RawMessage `json:"return"`
	Error  *struct {
		Class string `json:"class"`
		Desc  string `json:"desc"`
	} `json:"error"`
	Event string          `json:"event"`
	QMP   json.RawMessage `json:"QMP"`
}

// maxLine bounds one protocol line: QEMU's answers here are small (query-machines is the largest).
const maxLine = 1 << 20

func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		line = append(line, chunk...)
		if len(line) > maxLine {
			return nil, errors.New("qemu: protocol line over the size limit")
		}
		if !isPrefix {
			return line, nil
		}
	}
}

func deadline(ctx context.Context, conn net.Conn) func() {
	if d, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(d)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	return func() { stop() }
}

// NewQMP reads QEMU's greeting and leaves capabilities negotiation mode.
func NewQMP(ctx context.Context, conn net.Conn) (*QMP, error) {
	defer deadline(ctx, conn)()
	q := &QMP{conn: conn, r: bufio.NewReader(conn)}
	line, err := readLine(q.r)
	if err != nil {
		return nil, fmt.Errorf("qemu: QMP greeting: %w", err)
	}
	var greet reply
	if err := json.Unmarshal(line, &greet); err != nil || greet.QMP == nil {
		return nil, fmt.Errorf("qemu: not a QMP greeting: %.80q", line)
	}
	if err := q.exec("qmp_capabilities", nil, nil); err != nil {
		return nil, err
	}
	return q, nil
}

// Execute runs one command and decodes its return value into out (nil discards it).
func (q *QMP) Execute(ctx context.Context, cmd string, args, out any) error {
	defer deadline(ctx, q.conn)()
	return q.exec(cmd, args, out)
}

func (q *QMP) exec(cmd string, args, out any) error {
	req := map[string]any{"execute": cmd}
	if args != nil {
		req["arguments"] = args
	}
	b, _ := json.Marshal(req)
	if _, err := q.conn.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("qemu: QMP %s: %w", cmd, err)
	}
	for {
		line, err := readLine(q.r)
		if err != nil {
			return fmt.Errorf("qemu: QMP %s: %w", cmd, err)
		}
		var rep reply
		if err := json.Unmarshal(line, &rep); err != nil {
			return fmt.Errorf("qemu: QMP %s: %w", cmd, err)
		}
		if rep.Event != "" {
			continue
		}
		return decodeReply("QMP "+cmd, rep, out)
	}
}

func decodeReply(what string, rep reply, out any) error {
	if rep.Error != nil {
		return fmt.Errorf("qemu: %s: %s: %s", what, rep.Error.Class, rep.Error.Desc)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(rep.Return, out); err != nil {
		return fmt.Errorf("qemu: %s answer: %w", what, err)
	}
	return nil
}

// Close closes the connection.
func (q *QMP) Close() error { return q.conn.Close() }

// Probed is what a QMP session says about the QEMU it talks to.
type Probed struct {
	QEMUVersion string // e.g. 10.0.13
	MachineType string // the versioned machine the q35 alias names, e.g. pc-q35-10.0
}

// Probe asks QEMU whether it runs under KVM, its version and the versioned machine q35 stands for.
func (q *QMP) Probe(ctx context.Context) (Probed, error) {
	var kvm struct {
		Enabled bool `json:"enabled"`
		Present bool `json:"present"`
	}
	if err := q.Execute(ctx, "query-kvm", nil, &kvm); err != nil {
		return Probed{}, err
	}
	if !kvm.Enabled || !kvm.Present {
		return Probed{}, fmt.Errorf("qemu: KVM is not in use (present %t, enabled %t)", kvm.Present, kvm.Enabled)
	}
	var ver struct {
		QEMU struct{ Major, Minor, Micro int } `json:"qemu"`
	}
	if err := q.Execute(ctx, "query-version", nil, &ver); err != nil {
		return Probed{}, err
	}
	var machines []struct {
		Name  string `json:"name"`
		Alias string `json:"alias"`
	}
	if err := q.Execute(ctx, "query-machines", nil, &machines); err != nil {
		return Probed{}, err
	}
	p := Probed{QEMUVersion: fmt.Sprintf("%d.%d.%d", ver.QEMU.Major, ver.QEMU.Minor, ver.QEMU.Micro)}
	for _, m := range machines {
		if m.Alias == "q35" && strings.HasPrefix(m.Name, "pc-q35-") {
			p.MachineType = m.Name
		}
	}
	if p.MachineType == "" {
		return Probed{}, errors.New("qemu: no versioned machine answers to q35")
	}
	return p, nil
}

// QGA is one session with the QEMU guest agent (qemu-ga) in the guest.
type QGA struct {
	conn net.Conn
	r    *bufio.Reader
}

// NewQGA synchronises with the guest agent: a guest-sync answer with a fresh id proves every older
// answer still in the channel has been read past.
func NewQGA(ctx context.Context, conn net.Conn) (*QGA, error) {
	defer deadline(ctx, conn)()
	g := &QGA{conn: conn, r: bufio.NewReader(conn)}
	id := rand.Int64N(1 << 40)
	b, _ := json.Marshal(map[string]any{"execute": "guest-sync", "arguments": map[string]any{"id": id}})
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	for {
		line, err := readLine(g.r)
		if err != nil {
			return nil, fmt.Errorf("qemu: guest-sync: %w", err)
		}
		var rep reply
		if json.Unmarshal(line, &rep) != nil {
			continue
		}
		var got int64
		if json.Unmarshal(rep.Return, &got) == nil && got == id {
			return g, nil
		}
	}
}

// Execute runs one guest agent command.
func (g *QGA) Execute(ctx context.Context, cmd string, args, out any) error {
	defer deadline(ctx, g.conn)()
	req := map[string]any{"execute": cmd}
	if args != nil {
		req["arguments"] = args
	}
	b, _ := json.Marshal(req)
	if _, err := g.conn.Write(append(b, '\n')); err != nil {
		return err
	}
	line, err := readLine(g.r)
	if err != nil {
		return fmt.Errorf("qemu: %s: %w", cmd, err)
	}
	var rep reply
	if err := json.Unmarshal(line, &rep); err != nil {
		return fmt.Errorf("qemu: %s: %w", cmd, err)
	}
	return decodeReply(cmd, rep, out)
}

// SetTime sets the guest clock to t: after boot, and after any restore, whose clock is stale.
func (g *QGA) SetTime(ctx context.Context, t time.Time) error {
	return g.Execute(ctx, "guest-set-time", map[string]any{"time": t.UnixNano()}, nil)
}

// Close closes the connection.
func (g *QGA) Close() error { return g.conn.Close() }
