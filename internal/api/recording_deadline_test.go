// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// deadlineRecorder lets a ResponseRecorder take the upload route's read
// deadline; a recorder has no connection for one to bound.
type deadlineRecorder struct{ *httptest.ResponseRecorder }

func (deadlineRecorder) SetReadDeadline(time.Time) error { return nil }

func shortenUploadWindow(t *testing.T) {
	old := recordingUploadReadWindow
	recordingUploadReadWindow = 200 * time.Millisecond
	t.Cleanup(func() { recordingUploadReadWindow = old })
}

// boundedPut sends one PUT that must be answered within 10 s. A stalled body
// is a pipe nobody writes; it is closed if the bound passes, since a stalled
// request body otherwise pins the client transport.
func boundedPut(t *testing.T, client *http.Client, url, token string, body io.Reader, length int64) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if pr, ok := body.(*io.PipeReader); ok {
		defer context.AfterFunc(ctx, func() { _ = pr.CloseWithError(errors.New("no answer")) })()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = length
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil || ctx.Err() != nil {
		t.Fatalf("PUT %s got no answer within 10 s: %v", url, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// An upload whose body stalls holds a store save slot only until the read
// deadline on its connection. The server's Read of the socket really blocks:
// the client sends headers and no body. It fails, stores nothing, and frees
// its run and slot, so the run's next upload goes through.
func TestPGUploadRecording_ReadDeadlineEndsTricklingUpload(t *testing.T) {
	shortenUploadWindow(t)
	store := recording.NewPGStore(throwawayPGPool(t))
	h := newRecordingHarness(t, store, nil)
	srv := httptest.NewServer(panicFails(t, h.srv.Handler()))
	defer srv.Close()
	run := uuid.New()
	token := h.mintRunToken(t, run)
	base := srv.URL + "/api/v1/internal/recordings/" + run.String()
	for _, path := range []string{"", "/parts/2"} {
		body, writer := io.Pipe() // never written
		code := boundedPut(t, http.DefaultClient, base+path, token, body, 1<<20)
		_ = writer.Close()
		if code != http.StatusInternalServerError {
			t.Fatalf("stalled upload %q = %d, want the read-error 500", path, code)
		}
	}
	for _, key := range []string{run.String(), recording.CastKey(run.String(), recording.PartSuffix(2))} {
		if _, err := store.OpenCast(t.Context(), key); !errors.Is(err, recording.ErrNotFound) {
			t.Fatalf("stalled upload stored %q: %v", key, err)
		}
	}
	if code := boundedPut(t, http.DefaultClient, base, token, strings.NewReader("cast"), 4); code != http.StatusNoContent {
		t.Fatalf("upload after the stalled ones = %d", code)
	}
}

// The read window is one window from admission, not one per Read: a body that
// keeps trickling a byte every 100 ms still fails at the 200 ms deadline and
// stores nothing.
func TestPGUploadRecording_ReadDeadlineEndsSlowTrickle(t *testing.T) {
	shortenUploadWindow(t)
	store := recording.NewPGStore(throwawayPGPool(t))
	h := newRecordingHarness(t, store, nil)
	srv := httptest.NewServer(panicFails(t, h.srv.Handler()))
	defer srv.Close()
	run := uuid.New()
	body, writer := io.Pipe()
	stop := make(chan struct{})
	trickled := make(chan struct{})
	go func() {
		defer close(trickled)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if _, err := writer.Write([]byte("x")); err != nil {
					return
				}
			}
		}
	}()
	code := boundedPut(t, http.DefaultClient, srv.URL+"/api/v1/internal/recordings/"+run.String(), h.mintRunToken(t, run), body, 1<<20)
	close(stop)
	_ = writer.Close()
	<-trickled
	if code != http.StatusInternalServerError {
		t.Fatalf("trickling upload = %d, want the read-error 500", code)
	}
	if _, err := store.OpenCast(t.Context(), run.String()); !errors.Is(err, recording.ErrNotFound) {
		t.Fatalf("trickling upload stored a cast: %v", err)
	}
}

// readingStore reports each upload once the store has admitted it and starts
// reading its body.
type readingStore struct {
	*recording.PGStore
	reading chan string
}

type firstRead struct {
	io.Reader
	key  string
	ch   chan<- string
	seen bool
}

func (f *firstRead) Read(p []byte) (int, error) {
	if !f.seen {
		f.seen = true
		f.ch <- f.key
	}
	return f.Reader.Read(p)
}

func (s readingStore) SaveCast(ctx context.Context, key string, r io.Reader) error {
	return s.PGStore.SaveCast(ctx, key, &firstRead{Reader: r, key: key, ch: s.reading})
}

func (s readingStore) SaveCastNamed(ctx context.Context, run, suffix string, r io.Reader) error {
	return s.SaveCast(ctx, recording.CastKey(run, suffix), r)
}

// Two uploads whose raw TCP bodies stall take the upload save slot in turn,
// each ended by its read deadline. With one of them holding the slot, an
// attach-style save still completes on its own slot, and another run's upload
// gets its turn once the stalled read is ended.
func TestPGUploadRecording_StalledUploadsReleaseTheBudget(t *testing.T) {
	shortenUploadWindow(t)
	pg := recording.NewPGStore(throwawayPGPool(t))
	store := readingStore{PGStore: pg, reading: make(chan string, 8)}
	h := newRecordingHarness(t, store, nil)
	srv := httptest.NewServer(panicFails(t, h.srv.Handler()))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	var stalled []net.Conn
	for range 2 {
		run := uuid.New()
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_, _ = fmt.Fprintf(c, "PUT /api/v1/internal/recordings/%s HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer %s\r\nContent-Length: 1000\r\n\r\n", run, h.mintRunToken(t, run))
		stalled = append(stalled, c)
		<-store.reading // admitted (the second after the first's deadline); its Read is blocked on the socket
	}

	attach := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		attach <- pg.SaveCastNamed(ctx, uuid.NewString(), uuid.NewString(), bytes.NewReader([]byte("snapshot")))
	}()
	other := uuid.New()
	if code := boundedPut(t, http.DefaultClient, srv.URL+"/api/v1/internal/recordings/"+other.String(), h.mintRunToken(t, other), strings.NewReader("cast"), 4); code != http.StatusNoContent {
		t.Fatalf("another run's upload = %d", code)
	}
	if err := <-attach; err != nil {
		t.Fatalf("attach-style save = %v", err)
	}
	for _, c := range stalled {
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		line, _ := io.ReadAll(io.LimitReader(c, 12))
		if !strings.HasPrefix(string(line), "HTTP/1.1 500") {
			t.Fatalf("stalled upload answered %q, want the read-error 500", line)
		}
	}
}

// noDeadlineConn is a connection that refuses read deadlines.
type noDeadlineConn struct{ net.Conn }

func (noDeadlineConn) SetReadDeadline(time.Time) error {
	return errors.New("read deadlines unsupported")
}

type noDeadlineListener struct{ net.Listener }

func (l noDeadlineListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return noDeadlineConn{c}, nil
}

// An upload on a connection that cannot take a read deadline is refused
// before its body is read, and the refusal is audited.
func TestUploadRecording_RefusedWithoutReadDeadline(t *testing.T) {
	store := &fakeRecordingStore{}
	h := newRecordingHarness(t, store, nil)
	srv := httptest.NewUnstartedServer(panicFails(t, h.srv.Handler()))
	srv.Listener = noDeadlineListener{srv.Listener}
	srv.Start()
	defer srv.Close()
	run := uuid.New()
	code := boundedPut(t, http.DefaultClient, srv.URL+"/api/v1/internal/recordings/"+run.String(), h.mintRunToken(t, run), strings.NewReader("cast"), 4)
	if code != http.StatusInternalServerError {
		t.Fatalf("upload without a read deadline = %d, want 500", code)
	}
	if store.saved != nil {
		t.Fatalf("the store read %q", store.saved)
	}
	ev := lastAuditEvent(t, h.audit.snapshot(), "recording.upload")
	if ev.Outcome != "failure" || !strings.Contains(string(ev.Data), "read deadline") {
		t.Fatalf("refusal audit = %s %s", ev.Outcome, ev.Data)
	}
}

// blockedSaveStore holds every save until its context ends.
type blockedSaveStore struct{ fakeRecordingStore }

func (blockedSaveStore) SaveCastNamed(ctx context.Context, _, _ string, _ io.Reader) error {
	<-ctx.Done()
	return ctx.Err()
}

// A session recording save waits at most sessionRecordingSaveWait, even on the
// daemon-lifetime context, and its audit row is still written.
func TestSessionRecorder_SaveWaitIsBounded(t *testing.T) {
	old := sessionRecordingSaveWait
	sessionRecordingSaveWait = 50 * time.Millisecond
	t.Cleanup(func() { sessionRecordingSaveWait = old })
	audit := &recRecorder{}
	srv := New(Config{RecordingStore: &blockedSaveStore{}, Audit: audit, AdminToken: adminToken})
	tee, finish := srv.newSessionRecorder(types.AgentRun{ID: uuid.New()}, "ssh-"+uuid.NewString(), runner.AttachOptions{Cols: 80, Rows: 24})
	_, _ = tee.Write([]byte("output\r\n"))
	done := make(chan struct{})
	go func() { finish(context.Background(), types.ActorHuman, "alice@example.com"); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("finish waited on the store past its bound")
	}
	ev := lastAuditEvent(t, audit.snapshot(), "session.recording.write")
	if ev.Outcome != "failure" || !strings.Contains(string(ev.Data), "deadline exceeded") {
		t.Fatalf("session recording audit = %s %s", ev.Outcome, ev.Data)
	}
}

// savedStore reports each save's result.
type savedStore struct {
	*recording.PGStore
	saved chan error
}

func (s savedStore) SaveCast(ctx context.Context, key string, r io.Reader) error {
	err := s.PGStore.SaveCast(ctx, key, r)
	s.saved <- err
	return err
}

// The relayed upload door takes the same read deadline, over the real relay
// listener and runner stream: a runner host's stalled body fails the save with
// a read timeout, stores nothing and is audited, and the run's next relayed
// upload goes through. (The relay resets the stream when the daemon closes the
// connection, so the client sees the connection end, not the 500.)
func TestRelay_RecordingUploadReadDeadline(t *testing.T) {
	shortenUploadWindow(t)
	f := newAdmissionFixture(t)
	store := savedStore{PGStore: recording.NewPGStore(f.pool), saved: make(chan error, 4)}
	cfg := f.srv.cfg
	cfg.RecordingStore = store // the upload routes exist only with a store
	f.srv = New(cfg)
	caPEM, serving := hopServingTLS(t)
	relaySrv := f.srv.ServeRelay(serving)
	t.Cleanup(func() { _ = relaySrv.Close() })
	s := f.connect(t, nil, f.liveRow())
	if !f.waitReady(s) {
		t.Fatalf("the runner was not admitted: %v", s.result())
	}
	s.mu.Lock()
	peer := s.peer
	s.mu.Unlock()
	client := runnerRelayClient(t, peer, caPEM, io.Discard)
	token := runToken(t, f.srv, f.run.ID)
	url := "https://" + relayHost + ":8443/api/v1/internal/recordings/" + f.run.ID.String()

	body, writer := io.Pipe() // never written
	req, err := http.NewRequest(http.MethodPut, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = 1 << 20
	req.Header.Set("Authorization", "Bearer "+token)
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		if resp, err := client.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case err := <-store.saved:
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("stalled relayed save = %v, want a read timeout", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stalled relayed save outlived its read deadline")
	}
	_ = writer.Close() // let the client's request writer finish
	<-answered
	if _, err := store.OpenCast(t.Context(), f.run.ID.String()); !errors.Is(err, recording.ErrNotFound) {
		t.Fatalf("stalled relayed upload stored a cast: %v", err)
	}
	if ev, ok := relayAudit(f.srv, f.run.ID, "recording.upload"); !ok || ev.Outcome != "failure" || !strings.Contains(string(ev.Data), "timeout") {
		t.Fatalf("stalled relayed upload audit = %v %+v", ok, ev)
	}
	if code := boundedPut(t, client, url, token, strings.NewReader("cast"), 4); code != http.StatusNoContent {
		t.Fatalf("relayed upload after the stalled one = %d", code)
	}
}
