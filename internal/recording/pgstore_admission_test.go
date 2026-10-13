// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

var errGate = errors.New("gate closed")

// gateReader announces its first Read on entered, then blocks until release
// closes and fails, so its save ends before touching the database.
type gateReader struct {
	id      int
	entered chan<- int
	release chan struct{}
	seen    bool
}

func (g *gateReader) Read([]byte) (int, error) {
	if !g.seen {
		g.seen = true
		g.entered <- g.id
	}
	<-g.release
	return 0, errGate
}

// gatedSaves runs saves against a pool-less PGStore and counts admission waits.
type gatedSaves struct {
	t       *testing.T
	s       *recording.PGStore
	entered chan int
	waits   chan struct{}
	done    chan error
	gates   []chan struct{}
	pending int
}

func newGatedSaves(t *testing.T) *gatedSaves {
	g := &gatedSaves{t: t, s: recording.NewPGStore(nil), entered: make(chan int, 64), waits: make(chan struct{}, 64), done: make(chan error, 64)}
	recording.OnCastSaveWait(t, func() { g.waits <- struct{}{} })
	t.Cleanup(func() { // runs before the hook is restored
		for _, c := range g.gates {
			select {
			case <-c:
			default:
				close(c)
			}
		}
		for ; g.pending > 0; g.pending-- {
			<-g.done
		}
	})
	return g
}

// start launches save with a fresh gate reader and returns its id.
func (g *gatedSaves) start(save func(r io.Reader) error) int {
	id := len(g.gates)
	r := &gateReader{id: id, entered: g.entered, release: make(chan struct{})}
	g.gates = append(g.gates, r.release)
	g.pending++
	go func() { g.done <- save(r) }()
	return id
}

// finish releases save id and waits for one save to return.
func (g *gatedSaves) finish(id int) error {
	close(g.gates[id])
	g.pending--
	return <-g.done
}

// fill starts one SaveCast per stream slot, each for its own run, and waits
// until each is reading.
func (g *gatedSaves) fill() []int {
	var ids []int
	for range recording.CastSaveSlots {
		run := uuid.NewString()
		ids = append(ids, g.start(func(r io.Reader) error { return g.s.SaveCast(g.t.Context(), run, r) }))
	}
	for range ids {
		<-g.entered
	}
	return ids
}

// expectWait requires the latest save to queue for a slot rather than read.
func (g *gatedSaves) expectWait(what string) {
	g.t.Helper()
	select {
	case <-g.waits:
	case id := <-g.entered:
		g.t.Fatalf("%s (save %d) read its stream with every slot held", what, id)
	}
}

func TestPGStore_SaveAdmissionQueuesOverBudget(t *testing.T) {
	g := newGatedSaves(t)
	held := g.fill()
	attach := g.start(func(r io.Reader) error {
		return g.s.SaveCastNamed(t.Context(), uuid.NewString(), uuid.NewString(), r)
	})
	g.expectWait("named save")
	part := g.start(func(r io.Reader) error {
		return g.s.SaveCastNamed(t.Context(), uuid.NewString(), recording.PartSuffix(2), r)
	})
	g.expectWait("part save")
	if n := recording.CastSaveSlotsHeld(); n != recording.CastSaveSlots {
		t.Fatalf("slots held = %d, want %d", n, recording.CastSaveSlots)
	}
	if recording.InMemorySaveSlotHeld() {
		t.Fatal("a stream save took the in-memory slot")
	}

	// Each finished save admits exactly one waiter.
	if err := g.finish(held[0]); !errors.Is(err, errGate) {
		t.Fatalf("finished save = %v", err)
	}
	first := <-g.entered
	if first != attach && first != part {
		t.Fatalf("save %d entered, want a queued one", first)
	}
	if err := g.finish(held[1]); !errors.Is(err, errGate) {
		t.Fatalf("finished save = %v", err)
	}
	second := <-g.entered
	if second == first || (second != attach && second != part) {
		t.Fatalf("save %d entered after %d", second, first)
	}
	for _, id := range []int{first, second} {
		if err := g.finish(id); !errors.Is(err, errGate) {
			t.Fatalf("queued save = %v", err)
		}
	}
	if n := recording.CastSaveSlotsHeld(); n != 0 {
		t.Fatalf("slots held after every save returned = %d", n)
	}
}

func TestPGStore_SaveAdmissionCancelLeavesNoReservation(t *testing.T) {
	g := newGatedSaves(t)
	run := uuid.NewString()
	held := g.fill()
	ctx, cancel := context.WithCancel(t.Context())
	g.start(func(r io.Reader) error { return g.s.SaveCastNamed(ctx, run, "ssh-"+uuid.NewString(), r) })
	g.expectWait("cancelled save")
	cancel()
	g.pending--
	if err := <-g.done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter = %v, want context.Canceled", err)
	}
	if n := recording.CastSaveSlotsHeld(); n != recording.CastSaveSlots {
		t.Fatalf("slots held after a waiter left = %d, want %d", n, recording.CastSaveSlots)
	}
	for _, id := range held {
		if err := g.finish(id); !errors.Is(err, errGate) {
			t.Fatalf("finished save = %v", err)
		}
	}
	if n := recording.CastSaveSlotsHeld(); n != 0 {
		t.Fatalf("slots held after every save returned = %d", n)
	}
	// A fresh save of the same run is admitted at once: nothing was left reserved.
	id := g.start(func(r io.Reader) error { return g.s.SaveCast(t.Context(), run, r) })
	select {
	case got := <-g.entered:
		if got != id {
			t.Fatalf("save %d entered, want %d", got, id)
		}
	case <-g.waits:
		t.Fatal("the cancelled waiter left its run or a slot held")
	}
	if err := g.finish(id); !errors.Is(err, errGate) {
		t.Fatalf("fresh save = %v", err)
	}
	if len(g.waits) != 0 {
		t.Fatal("a save waited with every slot free")
	}
}

// heldReader records how many slots were held when its stream was first read.
type heldReader struct {
	r    io.Reader
	held int
}

func (h *heldReader) Read(p []byte) (int, error) {
	if h.held < 0 {
		h.held = recording.CastSaveSlotsHeld()
	}
	return h.r.Read(p)
}

func TestPGStore_SaveAdmissionSmallAndMaxStreams(t *testing.T) {
	s := recording.NewPGStore(pgPool(t))
	run := uuid.NewString()
	const maxCast = 64 << 20
	for _, size := range []int{10, 64<<10 + 1, maxCast, maxCast + 1} {
		h := &heldReader{r: bytes.NewReader(make([]byte, size)), held: -1}
		err := s.SaveCastNamed(t.Context(), run, uuid.NewString(), h)
		if (err != nil) != (size > maxCast) {
			t.Fatalf("save of %d bytes = %v", size, err)
		}
		if h.held != 1 {
			t.Errorf("%d-byte stream read with %d slots held, want 1", size, h.held)
		}
		if n := recording.CastSaveSlotsHeld(); n != 0 {
			t.Errorf("slots held after a %d-byte save = %d", size, n)
		}
	}
	if _, err := s.DeleteRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCast(t.Context(), run, bytes.NewReader([]byte("late"))); !errors.Is(err, recording.ErrErased) {
		t.Fatalf("save after erasure = %v", err)
	}
	if n := recording.CastSaveSlotsHeld(); n != 0 {
		t.Errorf("slots held after a refused save = %d", n)
	}
}

// One run holds at most one stream slot: its second save waits for its first,
// while another run's save is admitted.
func TestPGStore_SaveAdmissionOneSlotPerRun(t *testing.T) {
	g := newGatedSaves(t)
	run := uuid.NewString()
	first := g.start(func(r io.Reader) error { return g.s.SaveCast(t.Context(), run, r) })
	if got := <-g.entered; got != first {
		t.Fatalf("save %d entered, want %d", got, first)
	}
	second := g.start(func(r io.Reader) error {
		return g.s.SaveCastNamed(t.Context(), run, recording.PartSuffix(2), r)
	})
	g.expectWait("second save of one run")
	other := g.start(func(r io.Reader) error { return g.s.SaveCast(t.Context(), uuid.NewString(), r) })
	select {
	case got := <-g.entered:
		if got != other {
			t.Fatalf("save %d entered, want the other run's %d", got, other)
		}
	case <-g.waits:
		t.Fatal("another run's save waited behind this run")
	}
	if err := g.finish(first); !errors.Is(err, errGate) {
		t.Fatalf("first save = %v", err)
	}
	if got := <-g.entered; got != second {
		t.Fatalf("save %d entered, want the run's second save %d", got, second)
	}
	for _, id := range []int{second, other} {
		if err := g.finish(id); !errors.Is(err, errGate) {
			t.Fatalf("save %d = %v", id, err)
		}
	}
	if n := recording.CastSaveSlotsHeld(); n != 0 {
		t.Fatalf("slots held after every save returned = %d", n)
	}
}

// An in-memory save (an attach or SSH session snapshot) completes while
// trickling uploads hold every stream slot.
func TestPGStore_InMemorySaveDoesNotWaitBehindUploads(t *testing.T) {
	pg := recording.NewPGStore(pgPool(t))
	g := newGatedSaves(t)
	held := g.fill()
	run := uuid.NewString()
	done := make(chan error, 1)
	go func() {
		done <- pg.SaveCastNamed(t.Context(), run, "ssh-"+uuid.NewString(), bytes.NewReader([]byte("snapshot")))
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("in-memory save = %v", err)
		}
	case <-g.waits:
		t.Fatal("an in-memory save waited behind uploads")
	}
	if recording.InMemorySaveSlotHeld() {
		t.Fatal("in-memory slot still held after its save returned")
	}
	for _, id := range held {
		if err := g.finish(id); !errors.Is(err, errGate) {
			t.Fatalf("upload = %v", err)
		}
	}
}

// A save waits at most castSaveAdmitWait for admission, even on a context that
// never ends, and leaves holding nothing.
func TestPGStore_SaveAdmissionWaitIsBounded(t *testing.T) {
	recording.ShortenCastSaveAdmitWait(t, 50*time.Millisecond)
	g := newGatedSaves(t)
	held := g.fill()
	g.start(func(r io.Reader) error { return g.s.SaveCast(context.Background(), uuid.NewString(), r) })
	g.expectWait("save over the budget")
	select {
	case err := <-g.done:
		g.pending--
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("over-budget save = %v, want its admission wait to end", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("admission wait outlived its bound")
	}
	if n := recording.CastSaveSlotsHeld(); n != recording.CastSaveSlots {
		t.Fatalf("slots held = %d, want %d", n, recording.CastSaveSlots)
	}
	for _, id := range held {
		if err := g.finish(id); !errors.Is(err, errGate) {
			t.Fatalf("finished save = %v", err)
		}
	}
}
