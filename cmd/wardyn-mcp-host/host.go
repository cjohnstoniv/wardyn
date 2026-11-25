// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// caDir is where the CA files go, the paths the sandbox's CA trust variables name.
	caDir = "/tmp/wardyn"
	// listenAddr and mcpPath are the one endpoint the MCP proxy forwards to.
	listenAddr = ":8765"
	mcpPath    = "/mcp"
	// maxBody caps a POST from the proxy, which already caps what it checks at 1 MiB.
	maxBody       = 4 << 20
	sessionHeader = "Mcp-Session-Id"
	launchEnv     = "WARDYN_MCP_LAUNCH_JSON"
	caPEMEnv      = "WARDYN_MITM_CA_PEM"
)

// systemBundles are the system CA bundles, in the order the first present one is taken.
var systemBundles = []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/ssl/cert.pem", "/etc/pki/tls/certs/ca-bundle.crt"}

type options struct {
	caDir   string
	bundles []string
	listen  string
	env     func(string) string
	stderr  io.Writer
	// ready, when set, is told the bound address once the server is listening.
	ready func(addr string)
}

// launch is the server's command and arguments, run with no shell.
type launch struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

func parseLaunch(raw string) (launch, error) {
	var l launch
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		return launch{}, fmt.Errorf("%s: %w", launchEnv, err)
	}
	if l.Command == "" {
		return launch{}, fmt.Errorf("%s names no command", launchEnv)
	}
	return l, nil
}

// writeCAFiles writes dir/mitm-ca.pem (the proxy's CA) and dir/ca-bundle.pem
// (the first system bundle present, then the CA; the CA alone, with a warning,
// when there is none). Each file is renamed into place, so no reader sees part
// of one.
func writeCAFiles(dir, caPEM string, bundles []string, stderr io.Writer) error {
	if caPEM == "" {
		return fmt.Errorf("%s is empty: the server's TLS through its proxy could not be verified", caPEMEnv)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("make the CA directory: %w", err)
	}
	ca := []byte(caPEM)
	if !bytes.HasSuffix(ca, []byte("\n")) {
		ca = append(ca, '\n')
	}
	if err := writeFileAtomic(filepath.Join(dir, "mitm-ca.pem"), ca); err != nil {
		return err
	}
	bundle := ca
	for _, path := range bundles {
		sys, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if len(sys) > 0 && !bytes.HasSuffix(sys, []byte("\n")) {
			sys = append(sys, '\n')
		}
		bundle = append(sys, ca...)
		break
	}
	if len(bundle) == len(ca) {
		fmt.Fprintln(stderr, "wardyn-mcp-host: WARNING: no system CA bundle found; ca-bundle.pem is the proxy's CA alone, so hosts the proxy does not inspect will not verify")
	}
	return writeFileAtomic(filepath.Join(dir, "ca-bundle.pem"), bundle)
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// run writes the CA files, starts the server and serves it until it exits; the
// exit code is the server's. Nothing starts when the launch or a CA file fails.
func run(ctx context.Context, o options) (int, error) {
	l, err := parseLaunch(o.env(launchEnv))
	if err != nil {
		return 1, err
	}
	if err := writeCAFiles(o.caDir, o.env(caPEMEnv), o.bundles, o.stderr); err != nil {
		return 1, err
	}
	ln, err := net.Listen("tcp", o.listen)
	if err != nil {
		return 1, err
	}
	h := &host{pending: map[string]chan json.RawMessage{}, streams: map[chan json.RawMessage]struct{}{}, done: make(chan struct{})}
	cmd := exec.Command(l.Command, l.Args...)
	cmd.Stderr = o.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = ln.Close()
		return 1, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = ln.Close()
		return 1, err
	}
	if err := cmd.Start(); err != nil {
		_ = ln.Close()
		return 1, fmt.Errorf("start the MCP server: %w", err)
	}
	h.stdin = stdin
	h.started.Store(true)
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	if o.ready != nil {
		o.ready(ln.Addr().String())
	}
	readDone := make(chan struct{})
	go func() { defer close(readDone); h.readServer(stdout) }()
	go func() {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Signal(syscall.SIGTERM)
		case <-h.done:
		}
	}()
	<-readDone // the server's stdout closes as it exits; Wait must follow the last read
	werr := cmd.Wait()
	close(h.done)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	if ee := (*exec.ExitError)(nil); errors.As(werr, &ee) {
		return max(ee.ExitCode(), 1), nil
	}
	return 0, werr
}

// host relays streamable HTTP to the server's stdio: each message the proxy
// posts is one line on stdin; each line on stdout is an answer to a waiting
// POST, matched by id, or else a message for the open GET streams.
type host struct {
	stdin   io.Writer
	writeMu sync.Mutex
	started atomic.Bool
	done    chan struct{}

	mu      sync.Mutex
	pending map[string]chan json.RawMessage
	streams map[chan json.RawMessage]struct{}
	session string
}

func (h *host) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		if r.Method != http.MethodGet || !h.started.Load() {
			http.Error(w, "not started", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
		return
	case mcpPath:
	default:
		http.NotFound(w, r)
		return
	}
	if id := r.Header.Get(sessionHeader); id != "" {
		h.mu.Lock()
		known := id == h.session
		h.mu.Unlock()
		if !known {
			http.Error(w, "unknown MCP session", http.StatusNotFound)
			return
		}
	}
	switch r.Method {
	case http.MethodPost:
		h.post(w, r)
	case http.MethodGet:
		h.stream(w, r)
	default:
		// A session ends with the server: one process is one session.
		http.Error(w, "this MCP server's session ends when its sandbox does", http.StatusMethodNotAllowed)
	}
}

// rpcMessage is what routing needs of a JSON-RPC message.
type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Method *string         `json:"method"`
}

func (h *host) post(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		http.Error(w, "read the MCP request body", http.StatusBadRequest)
		return
	}
	body = bytes.TrimSpace(body)
	batch := len(body) > 0 && body[0] == '['
	raws := []json.RawMessage{body}
	if batch {
		raws = nil
		if json.Unmarshal(body, &raws) != nil || len(raws) == 0 {
			http.Error(w, "not a JSON-RPC batch", http.StatusBadRequest)
			return
		}
	}
	var keys []string
	var waits []chan json.RawMessage
	initialize := false
	defer func() { h.forget(keys) }()
	for _, raw := range raws {
		var m rpcMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			http.Error(w, "not a JSON-RPC message", http.StatusBadRequest)
			return
		}
		if m.Method == nil || len(m.ID) == 0 || string(m.ID) == "null" {
			continue // a notification or an answer: nothing comes back for it
		}
		initialize = initialize || *m.Method == "initialize"
		ch, ok := h.await(idKey(m.ID))
		if !ok {
			http.Error(w, "a request with this id is already waiting", http.StatusConflict)
			return
		}
		keys, waits = append(keys, idKey(m.ID)), append(waits, ch)
	}
	if err := h.send(raws); err != nil {
		http.Error(w, "the MCP server is not reading", http.StatusBadGateway)
		return
	}
	if len(waits) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	answers := make([]json.RawMessage, 0, len(waits))
	for _, ch := range waits {
		select {
		case a := <-ch:
			answers = append(answers, a)
		case <-r.Context().Done():
			return
		case <-h.done:
			http.Error(w, "the MCP server exited", http.StatusBadGateway)
			return
		}
	}
	if initialize {
		w.Header().Set(sessionHeader, h.sessionID())
	}
	w.Header().Set("Content-Type", "application/json")
	if batch {
		_ = json.NewEncoder(w).Encode(answers)
		return
	}
	_, _ = w.Write(append(answers[0], '\n'))
}

// sessionID is the one session of this server, made on its first initialize.
func (h *host) sessionID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.session == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		h.session = hex.EncodeToString(b)
	}
	return h.session
}

func (h *host) stream(w http.ResponseWriter, r *http.Request) {
	ch := make(chan json.RawMessage, 64)
	h.mu.Lock()
	h.streams[ch] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.streams, ch)
		h.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	if fl != nil {
		fl.Flush()
	}
	for {
		select {
		case m := <-ch:
			if _, err := fmt.Fprintf(w, "event: message\ndata: %s\n\n", m); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		case <-r.Context().Done():
			return
		case <-h.done:
			return
		}
	}
}

// send writes each message to the server as one line.
func (h *host) send(raws []json.RawMessage) error {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	for _, raw := range raws {
		var line bytes.Buffer
		if err := json.Compact(&line, raw); err != nil {
			return err
		}
		line.WriteByte('\n')
		if _, err := h.stdin.Write(line.Bytes()); err != nil {
			return err
		}
	}
	return nil
}

func (h *host) await(key string) (chan json.RawMessage, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, dup := h.pending[key]; dup {
		return nil, false
	}
	ch := make(chan json.RawMessage, 1)
	h.pending[key] = ch
	return ch, true
}

func (h *host) forget(keys []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, k := range keys {
		delete(h.pending, k)
	}
}

// readServer routes each line the server writes until its stdout closes.
func (h *host) readServer(stdout io.Reader) {
	rd := bufio.NewReaderSize(stdout, 64<<10)
	for {
		line, err := rd.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			h.route(json.RawMessage(bytes.Clone(line)))
		}
		if err != nil {
			return
		}
	}
}

func (h *host) route(msg json.RawMessage) {
	var m rpcMessage
	if json.Unmarshal(msg, &m) != nil {
		return // not JSON-RPC: a server's stray output is not relayed
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if m.Method == nil && len(m.ID) > 0 {
		if ch, ok := h.pending[idKey(m.ID)]; ok {
			delete(h.pending, idKey(m.ID))
			ch <- msg
			return
		}
	}
	for ch := range h.streams {
		select {
		case ch <- msg:
		default: // a stream that is not reading loses the message, never the server's progress
		}
	}
}

// idKey is a JSON-RPC id as a map key: its compact JSON, so 1 and "1" differ.
func idKey(id json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, id) != nil {
		return string(id)
	}
	return b.String()
}
