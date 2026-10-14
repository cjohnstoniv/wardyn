// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
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
	holding := held
	var admitted []int
	for len(admitted) < 2 {
		if err := g.finish(holding[0]); !errors.Is(err, errGate) {
			t.Fatalf("finished save = %v", err)
		}
		id := <-g.entered
		if (id != attach && id != part) || slices.Contains(admitted, id) {
			t.Fatalf("save %d entered, want a queued one not yet admitted (%v)", id, admitted)
		}
		if n := recording.CastSaveSlotsHeld(); n != recording.CastSaveSlots {
			t.Fatalf("slots held = %d, want %d", n, recording.CastSaveSlots)
		}
		admitted, holding = append(admitted, id), append(holding[1:], id)
	}
	for _, id := range holding {
		if err := g.finish(id); !errors.Is(err, errGate) {
			t.Fatalf("queued save = %v", err)
		}
	}
	if n := recording.CastSaveSlotsHeld(); n != 0 {
		t.Fatalf("slots held after every save returned = %d", n)
	}
}

// Uploads are written one at a time: while one run's stream save holds the
// slot, another run's waits, and is admitted when the first returns.
func TestPGStore_UploadsAreWrittenOneAtATime(t *testing.T) {
	g := newGatedSaves(t)
	first := g.start(func(r io.Reader) error { return g.s.SaveCast(t.Context(), uuid.NewString(), r) })
	if got := <-g.entered; got != first {
		t.Fatalf("save %d entered, want %d", got, first)
	}
	second := g.start(func(r io.Reader) error { return g.s.SaveCast(t.Context(), uuid.NewString(), r) })
	g.expectWait("a second run's upload")
	if recording.InMemorySaveSlotHeld() {
		t.Fatal("an upload took the in-memory slot")
	}
	if err := g.finish(first); !errors.Is(err, errGate) {
		t.Fatalf("first upload = %v", err)
	}
	if got := <-g.entered; got != second {
		t.Fatalf("save %d entered, want the waiting upload %d", got, second)
	}
	if err := g.finish(second); !errors.Is(err, errGate) {
		t.Fatalf("second upload = %v", err)
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

// One group's stream saves go one at a time whatever the slot count: a second
// hold of the same group waits for the first, another group's does not, and a
// waiter that gives up leaves the group free.
func TestHoldGroup_OnePerGroup(t *testing.T) {
	waits := make(chan struct{}, 4)
	recording.OnCastSaveWait(t, func() { waits <- struct{}{} })
	run := uuid.NewString()
	release, err := recording.HoldGroup(t.Context(), run)
	if err != nil {
		t.Fatal(err)
	}
	releaseOther, err := recording.HoldGroup(t.Context(), uuid.NewString())
	if err != nil || len(waits) != 0 {
		t.Fatalf("another run's hold = %v after %d waits; want it at once", err, len(waits))
	}
	releaseOther()

	ctx, cancel := context.WithCancel(t.Context())
	gaveUp := make(chan error, 1)
	go func() { _, err := recording.HoldGroup(ctx, run); gaveUp <- err }()
	<-waits
	cancel()
	if err := <-gaveUp; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled second hold = %v", err)
	}

	next := make(chan func(), 1)
	go func() {
		r, err := recording.HoldGroup(t.Context(), run)
		if err != nil {
			t.Error(err)
		}
		next <- r
	}()
	<-waits
	release()
	(<-next)()
	again, err := recording.HoldGroup(t.Context(), run)
	if err != nil || len(waits) != 0 {
		t.Fatalf("hold after every release = %v after %d waits", err, len(waits))
	}
	again()
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

// A reader that reports its length is buffered at that length plus one; a
// stream keeps the 64 KiB start and the single regrow to the cap.
func TestReadCast_BufferFollowsAKnownLength(t *testing.T) {
	const maxCast = 64 << 20
	for _, n := range []int{0, 100, 8 << 20} {
		buf, err := recording.ReadCast(bytes.NewReader(make([]byte, n)))
		if err != nil || len(buf) != n || cap(buf) != n+1 {
			t.Errorf("%d-byte in-memory reader: len %d, cap %d, %v; want cap %d", n, len(buf), cap(buf), err, n+1)
		}
	}
	if buf, err := recording.ReadCast(bytes.NewReader(make([]byte, maxCast+10))); err != nil || len(buf) != maxCast+1 {
		t.Errorf("over-cap in-memory reader: len %d, %v; want the cap plus one", len(buf), err)
	}
	stream := func(n int) io.Reader {
		return io.LimitReader(&heldReader{r: bytes.NewReader(make([]byte, n))}, int64(n))
	}
	if buf, err := recording.ReadCast(stream(100)); err != nil || len(buf) != 100 || cap(buf) != 64<<10 {
		t.Errorf("small stream: len %d, cap %d, %v; want cap %d", len(buf), cap(buf), err, 64<<10)
	}
	if buf, err := recording.ReadCast(stream(64<<10 + 1)); err != nil || len(buf) != 64<<10+1 || cap(buf) != maxCast+1 {
		t.Errorf("stream past 64 KiB: len %d, cap %d, %v; want one regrow to %d", len(buf), cap(buf), err, maxCast+1)
	}
}

// A stream save queues in the group its context names, and in its run's by default.
func TestAdmissionGroup_DefaultsToTheRun(t *testing.T) {
	run := uuid.NewString()
	for _, key := range []string{run, run + "~part-2", run + "~ssh-" + uuid.NewString()} {
		if got := recording.AdmissionGroup(t.Context(), key); got != run {
			t.Errorf("default group of %q = %q, want the run", key, got)
		}
		if got := recording.AdmissionGroup(recording.WithAdmissionGroup(t.Context(), ""), key); got != run {
			t.Errorf("empty group of %q = %q, want the run", key, got)
		}
		if got := recording.AdmissionGroup(recording.WithAdmissionGroup(t.Context(), "owner:x"), key); got != "owner:x" {
			t.Errorf("named group of %q = %q", key, got)
		}
	}
}

// One owner's uploads queue behind each other before they queue for the slot,
// so another owner's upload is behind one holder at most: over three cycles
// of one owner's back-to-back uploads it is admitted as soon as the holder
// returns, well inside its admission wait.
func TestPGStore_OneOwnersUploadsDoNotCrowdOutAnother(t *testing.T) {
	recording.ShortenCastSaveAdmitWait(t, 2*time.Second)
	g := newGatedSaves(t)
	busy := recording.WithAdmissionGroup(t.Context(), "owner:busy")
	other := recording.WithAdmissionGroup(t.Context(), "owner:other")
	upload := func(ctx context.Context) int {
		return g.start(func(r io.Reader) error { return g.s.SaveCast(ctx, uuid.NewString(), r) })
	}
	holder := upload(busy)
	if got := <-g.entered; got != holder {
		t.Fatalf("save %d entered, want %d", got, holder)
	}
	for cycle := 1; cycle <= 3; cycle++ {
		next := upload(busy) // the busy owner's next run
		g.expectWait("the busy owner's next upload")
		theirs := upload(other)
		g.expectWait("another owner's upload")
		if err := g.finish(holder); !errors.Is(err, errGate) {
			t.Fatalf("cycle %d: holder = %v", cycle, err)
		}
		if got := <-g.entered; got != theirs {
			// The busy owner's next upload holds the slot now; the other
			// owner's waits out its admission bound behind it.
			g.pending--
			t.Fatalf("cycle %d: save %d was admitted ahead of the other owner's upload, which then ended with: %v", cycle, got, <-g.done)
		}
		if err := g.finish(theirs); !errors.Is(err, errGate) {
			t.Fatalf("cycle %d: the other owner's upload = %v", cycle, err)
		}
		if got := <-g.entered; got != next {
			t.Fatalf("cycle %d: save %d entered, want the busy owner's next %d", cycle, got, next)
		}
		// next may also have waited for the slot once its group was free.
		// It reads now, so that wait is over and every other save has
		// returned: drop it, or it stands in for a wait of the next cycle.
		for len(g.waits) > 0 {
			<-g.waits
		}
		holder = next
	}
	if err := g.finish(holder); !errors.Is(err, errGate) {
		t.Fatalf("last holder = %v", err)
	}
}

// pausedWaits makes each save that is about to wait pause until the test lets
// it go, and hands the test that save's go-ahead.
func pausedWaits(t *testing.T) chan chan struct{} {
	paused := make(chan chan struct{}, 8)
	recording.OnCastSaveWait(t, func() {
		goAhead := make(chan struct{})
		paused <- goAhead
		<-goAhead
	})
	return paused
}

type taken struct {
	name    string
	release func()
	err     error
}

// A released slot goes to the oldest waiter, never to a save arriving at that
// instant, and a waiter that gives up leaves the queue to the next.
func TestSlotQueue_FirstComeFirstServed(t *testing.T) {
	paused := pausedWaits(t)
	q := recording.NewSlotQueue(1)
	got := make(chan taken, 8)
	take := func(ctx context.Context, name string) {
		go func() { r, err := q.Take(ctx); got <- taken{name, r, err} }()
	}
	release, err := q.Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctxB, cancelB := context.WithCancel(t.Context())
	var goAheads []chan struct{}
	for _, w := range []struct {
		name string
		ctx  context.Context
	}{{"a", t.Context()}, {"b", ctxB}, {"c", t.Context()}} {
		take(w.ctx, w.name)
		goAheads = append(goAheads, <-paused) // w is about to wait
	}

	// Release while every waiter is paused between finding the slot held and
	// blocking: a newcomer must queue behind them, not take the slot.
	release()
	take(t.Context(), "newcomer")
	select {
	case goAhead := <-paused:
		goAheads = append(goAheads, goAhead)
	case w := <-got:
		t.Fatalf("%s took the released slot ahead of three waiters", w.name)
	}
	for _, g := range goAheads {
		close(g)
	}
	first := <-got
	if first.name != "a" || first.err != nil {
		t.Fatalf("released slot went to %s (%v), want the oldest waiter a", first.name, first.err)
	}

	cancelB()
	if w := <-got; w.name != "b" || !errors.Is(w.err, context.Canceled) {
		t.Fatalf("after b's cancel: %s returned %v", w.name, w.err)
	}
	holder := first
	for _, want := range []string{"c", "newcomer"} {
		holder.release()
		if holder = <-got; holder.name != want || holder.err != nil {
			t.Fatalf("slot went to %s (%v), want %s", holder.name, holder.err, want)
		}
	}
	holder.release()
	if n := q.Held(); n != 0 {
		t.Fatalf("slots held after every holder released = %d", n)
	}
}

// A waiter handed the slot just as its context ends either keeps it or passes
// it to the next waiter; the slot is never lost.
func TestSlotQueue_HandOffAtCancelIsNotLost(t *testing.T) {
	paused := pausedWaits(t)
	q := recording.NewSlotQueue(1)
	got := make(chan taken, 2)
	release, err := q.Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctxA, cancelA := context.WithCancel(t.Context())
	go func() { r, err := q.Take(ctxA); got <- taken{"a", r, err} }()
	goA := <-paused
	go func() { r, err := q.Take(t.Context()); got <- taken{"b", r, err} }()
	goB := <-paused

	release() // handed to a, which is still paused
	cancelA()
	close(goA)
	a := <-got
	switch {
	case a.err == nil:
		a.release()
	case !errors.Is(a.err, context.Canceled):
		t.Fatalf("a = %v", a.err)
	}
	close(goB)
	b := <-got
	if b.name != "b" || b.err != nil {
		t.Fatalf("%s = %v, want b admitted", b.name, b.err)
	}
	b.release()
	if n := q.Held(); n != 0 {
		t.Fatalf("slots held after every holder released = %d", n)
	}
}
