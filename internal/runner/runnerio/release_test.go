// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

type openLog struct {
	sub     *remote.Substrate
	mu      sync.Mutex
	targets []string
}

func (h *openLog) HandleEvent(ev runnerwire.Event) { h.sub.HandleEvent(ev) }
func (h *openLog) HandleOpen(st *runnerwire.Stream, o runnerwire.Open) error {
	h.mu.Lock()
	h.targets = append(h.targets, o.Target)
	h.mu.Unlock()
	return h.sub.HandleOpen(st, o)
}

// completedOutput leaves a finished run's output bound to "local-<run>",
// acknowledged up to ack.
func completedOutput(t *testing.T, s *Server, id uuid.UUID, ack int64) {
	t.Helper()
	w, err := s.Output(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BindOutput(id, "local-"+id.String()); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write([]byte("tail")); err != nil {
		t.Fatal(err)
	}
	b := s.outputs[id]
	b.EndDrain(nil)
	if err = b.ack(ack); err != nil {
		t.Fatal(err)
	}
}

func TestOnlineSkipsDeliveredOutput(t *testing.T) {
	synctest.Test(t, onlineSkipsDeliveredOutput)
}

// Output the org has acknowledged to its end gets no OPEN on a reconnect;
// output it has not still does.
func onlineSkipsDeliveredOutput(t *testing.T) {
	server, err := New(runnertest.NewFake(), "laptop", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	delivered, pending := uuid.New(), uuid.New()
	completedOutput(t, server, delivered, 4)
	completedOutput(t, server, pending, 2)
	link := &remote.Link{}
	opens := &openLog{sub: remote.New("laptop", link, remote.Options{})}
	link.SetHandler(opens)
	ctx, cancel := context.WithCancel(t.Context())
	org, run := runnerwire.Loopback()
	op := link.Attach(ctx, org)
	var rp *runnerwire.Peer
	rp = runnerwire.NewPeer(run, runnerwire.PeerConfig{OnCall: func(ctx context.Context, id uint32, method string, raw json.RawMessage) (any, error) {
		return server.Call(ctx, rp, id, method, raw)
	}})
	go rp.Run(ctx)
	server.Online(rp)
	synctest.Wait()
	opens.mu.Lock()
	got := slices.Clone(opens.targets)
	opens.mu.Unlock()
	rp.Close()
	op.Close()
	server.Offline(rp)
	cancel()
	synctest.Wait()
	if !slices.Equal(got, []string{pending.String()}) {
		t.Fatalf("reconnect opened %v, want only %s", got, pending)
	}
}

func TestReleaseOutputOnlyWhenSettled(t *testing.T) {
	dir := t.TempDir()
	s, err := New(runnertest.NewFake(), uuid.NewString(), dir)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	ref := "local-" + id.String()
	w, err := s.Output(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BindOutput(id, ref); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write([]byte("tail")); err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseOutput(id, ref); err == nil {
		t.Fatal("released output a live producer is writing")
	}
	b := s.outputs[id]
	b.EndDrain(nil)
	if err = s.ReleaseOutput(id, ref); err == nil {
		t.Fatal("released output the org has not acknowledged")
	}
	if err = b.ack(4); err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseOutput(id, "other"); err == nil {
		t.Fatal("released output for another sandbox")
	}
	if len(s.outputs) != 1 {
		t.Fatal("a refused release forgot output")
	}
	if err = s.ReleaseOutput(id, ref); err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseOutput(id, ref); err != nil {
		t.Fatalf("repeated release: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "output"))
	if err != nil || len(entries) != 0 || len(s.outputs) != 0 {
		t.Fatalf("released output remains: %v %v", entries, err)
	}
	// A release interrupted after its rename leaves only the hidden name, which
	// the next start removes.
	left := filepath.Join(dir, "output", releasedPrefix+uuid.NewString())
	if err = os.MkdirAll(filepath.Join(left, "partial"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = New(runnertest.NewFake(), uuid.NewString(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(left); !os.IsNotExist(err) {
		t.Fatalf("interrupted release survived a restart: %v", err)
	}
}

// Hidden files are not run output; a damaged run directory stops the start
// with an error that names it and the way out.
func TestNewNamesDamagedOutputAndIgnoresHiddenFiles(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "output")
	if err := os.MkdirAll(out, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, ".DS_Store"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(runnertest.NewFake(), uuid.NewString(), dir); err != nil {
		t.Fatalf("hidden file refused the start: %v", err)
	}
	id := uuid.New()
	damaged := filepath.Join(out, id.String())
	if err := os.MkdirAll(damaged, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(damaged, partName(0)), []byte("orphan"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := New(runnertest.NewFake(), uuid.NewString(), dir)
	if err == nil || !strings.Contains(err.Error(), damaged) || !strings.Contains(err.Error(), "move") {
		t.Fatalf("damaged output error=%v", err)
	}
	if err = os.RemoveAll(damaged); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(out, "notes")
	if err = os.Mkdir(stray, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = New(runnertest.NewFake(), uuid.NewString(), dir); err == nil || !strings.Contains(err.Error(), stray) || !strings.Contains(err.Error(), "move") {
		t.Fatalf("unknown entry error=%v", err)
	}
}
