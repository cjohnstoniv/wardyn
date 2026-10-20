// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// cdp speaks the DevTools protocol over --remote-debugging-pipe: the browser
// reads fd 3 and writes fd 4, one JSON message per NUL. Never a debugging
// port: loopback is shared with the agent.
type cdp struct {
	mu   sync.Mutex
	w    io.Writer
	r    *bufio.Reader
	next int
}

func newCDP(w io.Writer, r io.Reader) *cdp {
	return &cdp{w: w, r: bufio.NewReaderSize(r, 1<<16)}
}

// maxCDPMessage bounds one message read from the browser.
const maxCDPMessage = 16 << 20

type cdpMessage struct {
	ID     int             `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

// call sends one browser-level command and returns its result, skipping events.
func (c *cdp) call(method string, params json.RawMessage) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	id := c.next
	msg, err := json.Marshal(cdpMessage{ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	if _, err := c.w.Write(append(msg, 0)); err != nil {
		return nil, err
	}
	for {
		line, err := readNUL(c.r)
		if err != nil {
			return nil, err
		}
		var in cdpMessage
		if json.Unmarshal(line, &in) != nil || in.ID != id {
			continue
		}
		if len(in.Error) > 0 {
			return nil, fmt.Errorf("%s: %s", method, in.Error)
		}
		return in.Result, nil
	}
}

func readNUL(r *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		chunk, err := r.ReadSlice(0)
		out = append(out, chunk...)
		if len(out) > maxCDPMessage {
			return nil, errors.New("devtools message too large")
		}
		if err == nil {
			return out[:len(out)-1], nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}

// ctlRequest is one line on a control socket: a browser-level CDP command.
type ctlRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type ctlReply struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// serveCtl answers ctlRequests on sock (mode 0600, inside a 0700 directory)
// until the listener closes.
func serveCtl(ln net.Listener, c *cdp) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
			var req ctlRequest
			if err := json.NewDecoder(io.LimitReader(conn, 1<<20)).Decode(&req); err != nil {
				return
			}
			res, err := c.call(req.Method, req.Params)
			rep := ctlReply{Result: res}
			if err != nil {
				rep.Error = err.Error()
			}
			_ = json.NewEncoder(conn).Encode(rep)
		}()
	}
}

func listenCtl(sock string) (net.Listener, error) {
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// ctlCall sends one command to the control socket at sock.
func ctlCall(sock, method string, params json.RawMessage) (json.RawMessage, error) {
	conn, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("the browser is not running: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if err := json.NewEncoder(conn).Encode(ctlRequest{Method: method, Params: params}); err != nil {
		return nil, err
	}
	var rep ctlReply
	if err := json.NewDecoder(io.LimitReader(conn, maxCDPMessage)).Decode(&rep); err != nil {
		return nil, err
	}
	if rep.Error != "" {
		return nil, errors.New(rep.Error)
	}
	return rep.Result, nil
}

// cmdCDP sends one browser-level command to the person's browser (as the
// person) or, with --agent, to the agent's (as the agent), and prints the result.
func cmdCDP(args []string, out io.Writer) error {
	sock := ctlSock
	if len(args) > 0 && args[0] == "--agent" {
		if err := checkAgentUID(); err != nil {
			return err
		}
		sock, args = agentCtlSock(), args[1:]
	} else if err := checkPerson(); err != nil {
		return err
	}
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: cdp [--agent] METHOD [PARAMS-JSON]")
	}
	var params json.RawMessage
	if len(args) == 2 {
		if !json.Valid([]byte(args[1])) {
			return errors.New("PARAMS must be JSON")
		}
		params = json.RawMessage(args[1])
	}
	res, err := ctlCall(sock, args[0], params)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\n", res)
	return err
}
