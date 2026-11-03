// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
)

// fakeScreen is a WindowScreen that draws nothing.
type fakeScreen struct {
	mu       sync.Mutex
	readyErr error
	openErr  error
	wins     []*fakeWindow
	live     int // the most windows open at once
	texts    []macvm.WindowText
	vms      []unsafe.Pointer
}

func (s *fakeScreen) Ready() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readyErr
}

func (s *fakeScreen) setReady(err error) { s.mu.Lock(); s.readyErr = err; s.mu.Unlock() }

func (s *fakeScreen) Open(vm unsafe.Pointer, text macvm.WindowText) (macvm.Window, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.openErr != nil {
		return nil, s.openErr
	}
	w := &fakeWindow{closed: make(chan struct{}), locked: make(chan struct{})}
	s.wins = append(s.wins, w)
	open := 0
	for _, o := range s.wins {
		select {
		case <-o.closed:
		default:
			open++
		}
	}
	s.live = max(s.live, open)
	s.texts = append(s.texts, text)
	s.vms = append(s.vms, vm)
	return w, nil
}

func (s *fakeScreen) opens() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.wins) }

func (s *fakeScreen) mostOpen() int { s.mu.Lock(); defer s.mu.Unlock(); return s.live }

func (s *fakeScreen) win(i int) *fakeWindow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wins[i]
}

type fakeWindow struct {
	mu       sync.Mutex
	closed   chan struct{}
	locked   chan struct{}
	once     sync.Once
	detached int // Close calls
}

func (w *fakeWindow) Closed() <-chan struct{} { return w.closed }
func (w *fakeWindow) Locked() <-chan struct{} { return w.locked }

// lockNotice is the Mac's lock notification: as the native window does, it marks the window locked, then tears
// it down.
func (w *fakeWindow) lockNotice() {
	close(w.locked)
	w.once.Do(func() { close(w.closed) })
}

func (w *fakeWindow) Close() {
	w.mu.Lock()
	w.detached++
	w.mu.Unlock()
	w.once.Do(func() { close(w.closed) })
}

// byPerson is the person clicking the window's close button.
func (w *fakeWindow) byPerson() { w.once.Do(func() { close(w.closed) }) }

func (w *fakeWindow) closes() int { w.mu.Lock(); defer w.mu.Unlock(); return w.detached }

// windowEngine builds machines that can give a view pointer; vmErr makes the pointer unreadable.
type windowEngine struct {
	*macvmtest.Engine
	vmErr error
	built int
}

func (e *windowEngine) NewMachine(p macvm.Plan) (macvm.Machine, error) {
	m, err := e.Engine.NewMachine(p)
	if err != nil {
		return nil, err
	}
	e.built++
	return &windowMachine{Machine: m.(*macvmtest.Machine), err: e.vmErr}, nil
}

type windowMachine struct {
	*macvmtest.Machine
	err error
}

var vmByte byte

func (m *windowMachine) ViewHandle() (unsafe.Pointer, error) {
	if m.err != nil {
		return nil, m.err
	}
	return unsafe.Pointer(&vmByte), nil
}

type windowRig struct {
	*runRig
	scr *fakeScreen
	eng *windowEngine
}

// startWindowRun starts a helper on the fake engine with the fake screen; running says whether to bring the VM up.
func startWindowRun(t *testing.T, running bool, mod func(*windowEngine, *fakeScreen)) *windowRig {
	t.Helper()
	w := &windowRig{scr: &fakeScreen{}, eng: &windowEngine{Engine: &macvmtest.Engine{}}}
	if mod != nil {
		mod(w.eng, w.scr)
	}
	t.Cleanup(macvm.SetWindowRecheck(20 * time.Millisecond))
	w.runRig = startRun(t, w.eng.Engine, func(_ *macvm.RunConfig, o *macvm.RunOptions) {
		o.Engine = w.eng
		o.Window = w.scr
	})
	if running {
		w.attach(t)
		waitState(t, w.cc, macvm.StateRunning)
	}
	return w
}

// client dials a fresh ctl.sock connection, closed with the test.
func (w *windowRig) client(t *testing.T) *macvm.ControlClient {
	t.Helper()
	cc, err := macvm.DialControl(context.Background(), filepath.Join(w.sock, macvm.ControlSock))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

// wantRefusal asserts a window refused with the code and sentence.
func wantRefusal(t *testing.T, err error, code, sentence string) {
	t.Helper()
	var rf *macvm.WindowRefusal
	if !errors.As(err, &rf) || rf.Code != code || rf.Sentence != sentence {
		t.Fatalf("window error %v, want refusal %s %q", err, code, sentence)
	}
}

func recv(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case r, ok := <-ch:
		if !ok {
			t.Fatal("the window's channel closed with no reason")
		}
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("the window never ended")
	}
	return ""
}

func waitWindow(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// An open or a close of a run window is published on the events stream with its grant, so runnerd can tell a
// window it did not grant.
func TestWindowPublishesEvents(t *testing.T) {
	w := startWindowRun(t, true, nil)
	evs, err := w.client(t).Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	next := func() macvm.Event {
		t.Helper()
		for {
			select {
			case ev, ok := <-evs:
				if !ok {
					t.Fatal("the events stream ended")
				}
				if ev.Type == macvm.EventWindow {
					return ev
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no window event")
			}
		}
	}
	ended, err := w.client(t).Window(context.Background(), "grant-9", "")
	if err != nil {
		t.Fatal(err)
	}
	if ev := next(); ev.Detail != macvm.EventWindowOpen || ev.Grant != "grant-9" || ev.Reason != "" {
		t.Fatalf("open event %+v", ev)
	}
	w.scr.win(0).byPerson()
	recv(t, ended)
	if ev := next(); ev.Detail != macvm.EventWindowClosedDetail || ev.Grant != "grant-9" || ev.Reason != macvm.WindowClosedPerson {
		t.Fatalf("closed event %+v", ev)
	}
	// A refused open publishes nothing: the next event is the reopen's.
	if _, err := w.client(t).Window(context.Background(), "grant-10", ""); err != nil {
		t.Fatal(err)
	}
	if ev := next(); ev.Detail != macvm.EventWindowOpen || ev.Grant != "grant-10" {
		t.Fatalf("reopen event %+v", ev)
	}
}

// The native window's close is seen by polling its state: watchWindow ends the window's channel when the
// state leaves "up", whoever closed it. With no lock notification, locked stays open.
func TestWindowWatcherSeesThePersonsClose(t *testing.T) {
	var state atomic.Int32
	state.Store(1)
	closed, locked := make(chan struct{}), make(chan struct{})
	macvm.WatchWindow(func() int { return int(state.Load()) }, func() int64 { return 4 }, 4, time.Millisecond, closed, locked)
	select {
	case <-closed:
		t.Fatal("closed while the window was up")
	case <-time.After(20 * time.Millisecond):
	}
	state.Store(0) // the delegate's teardown
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the person's close was never seen")
	}
	select {
	case <-locked:
		t.Fatal("a person's close read as a lock")
	default:
	}
}

// A lock notification counted after the open marks the native window locked, before (or without) its teardown;
// one counted before the open does not.
func TestWindowWatcherSeesALock(t *testing.T) {
	var state atomic.Int32
	var notices atomic.Int64
	state.Store(1)
	notices.Store(2)
	closed, locked := make(chan struct{}), make(chan struct{})
	macvm.WatchWindow(func() int { return int(state.Load()) }, notices.Load, 2, time.Millisecond, closed, locked)
	select {
	case <-locked:
		t.Fatal("locked with no lock since the open")
	case <-time.After(20 * time.Millisecond):
	}
	// The lock observer counts first, then tears down: the watcher must show locked by the time closed closes.
	notices.Add(1)
	state.Store(0)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the lock's teardown was never seen")
	}
	select {
	case <-locked:
	default:
		t.Fatal("a window a lock tore down closed without reading as locked")
	}
}

func TestWindowRefusedBeforeTheVMIsRunning(t *testing.T) {
	w := startWindowRun(t, false, nil)
	_, err := w.client(t).Window(context.Background(), "grant-1", "")
	wantRefusal(t, err, macvm.WindowCodeNotRunning, macvm.WindowNotRunningSentence)
	if w.scr.opens() != 0 {
		t.Fatal("a window opened on a VM that was not running")
	}
	// The refusal freed nothing it did not take: the window opens once the VM runs.
	w.attach(t)
	waitState(t, w.cc, macvm.StateRunning)
	if _, err := w.client(t).Window(context.Background(), "grant-1", ""); err != nil {
		t.Fatalf("after the VM runs: %v", err)
	}
}

func TestWindowRefusedToAStranger(t *testing.T) {
	w := startWindowRun(t, true, nil)
	t.Cleanup(macvm.SetControlOwnerUID(int64(os.Getuid() + 1)))
	cc := w.client(t)
	_, err := cc.Window(context.Background(), "grant-1", "")
	wantRefusal(t, err, macvm.WindowCodeNotOwner, macvm.WindowNotOwnerSentence)
	if w.scr.opens() != 0 {
		t.Fatal("a window opened for a stranger")
	}
	// The refusal ends the connection.
	if _, err := cc.Status(context.Background()); err == nil {
		t.Fatal("the connection outlived the refusal")
	}
}

func TestWindowEndsWhenTheConnectionCloses(t *testing.T) {
	w := startWindowRun(t, true, nil)
	cc := w.client(t)
	ended, err := cc.Window(context.Background(), "grant-1", "Run 7 for Sam")
	if err != nil {
		t.Fatal(err)
	}
	if w.scr.opens() != 1 || w.scr.texts[0].Title != "Run 7 for Sam" || w.scr.texts[0].Strip != macvm.WindowStrip || w.scr.vms[0] != unsafe.Pointer(&vmByte) {
		t.Fatalf("the window opened as %+v on %v", w.scr.texts, w.scr.vms)
	}
	_ = cc.Close()
	waitWindow(t, "the window to close", func() bool { return w.scr.win(0).closes() > 0 })
	if got := recv(t, ended); got != macvm.WindowClosedConnection {
		t.Fatalf("reason %q", got)
	}
	// Closing a window never stops the VM or the helper.
	if st := waitState(t, w.cc, macvm.StateRunning); st.HelperPID == 0 {
		t.Fatalf("status %+v", st)
	}
	// The slot is free: a new window opens.
	waitWindow(t, "the slot to free", func() bool {
		cc2 := w.client(t)
		_, err := cc2.Window(context.Background(), "grant-2", "")
		return err == nil
	})
}

func TestWindowCloseEndsTheConnectionWithItsReason(t *testing.T) {
	w := startWindowRun(t, true, nil)
	ended, err := w.client(t).Window(context.Background(), "grant-1", "")
	if err != nil {
		t.Fatal(err)
	}
	w.scr.win(0).byPerson()
	if got := recv(t, ended); got != macvm.WindowClosedPerson {
		t.Fatalf("reason %q, want %q", got, macvm.WindowClosedPerson)
	}
	if _, ok := <-ended; ok {
		t.Fatal("the connection went on after the window closed")
	}
	// The person's close only detached the view: the VM runs and the helper answers.
	waitState(t, w.cc, macvm.StateRunning)
	if w.scr.win(0).closes() != 1 {
		t.Fatalf("the window was detached %d times", w.scr.win(0).closes())
	}
}

func TestWindowSecondOpenIsRefused(t *testing.T) {
	w := startWindowRun(t, true, nil)
	t.Cleanup(macvm.SetWindowReleaseWait(time.Minute))
	first := w.client(t)
	if _, err := first.Window(context.Background(), "grant-1", ""); err != nil {
		t.Fatal(err)
	}
	// The first window's connection is live, so the refusal is at once: no wait for a release.
	start := time.Now()
	_, err := w.client(t).Window(context.Background(), "grant-2", "")
	wantRefusal(t, err, macvm.WindowCodeOpen, macvm.WindowAlreadyOpenSentence)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the refusal took %v", d)
	}
	if w.scr.opens() != 1 {
		t.Fatalf("%d windows opened", w.scr.opens())
	}
	// The first window is untouched by the refusal.
	if w.scr.win(0).closes() != 0 {
		t.Fatal("the refused open closed the first window")
	}
}

// holdRelease stops the first window to end just after its hold ends, before it is closed and its slot
// freed: its client has closed, and the helper has not let the window go. let releases it.
func holdRelease(t *testing.T) (reached <-chan struct{}, let func()) {
	t.Helper()
	at, hold := make(chan struct{}), make(chan struct{})
	var first, done sync.Once
	t.Cleanup(macvm.SetWindowReleaseHook(func() { first.Do(func() { close(at); <-hold }) }))
	let = func() { done.Do(func() { close(hold) }) }
	t.Cleanup(let)
	return at, let
}

// A reopen that arrives while the first window's client has closed, but before the helper has freed its slot,
// waits for that release and then opens: never two windows at once.
func TestWindowReopenWaitsForAnEndingWindow(t *testing.T) {
	w := startWindowRun(t, true, nil)
	t.Cleanup(macvm.SetWindowReleaseWait(time.Minute))
	reached, let := holdRelease(t)
	first := w.client(t)
	if _, err := first.Window(context.Background(), "grant-1", ""); err != nil {
		t.Fatal(err)
	}
	waiting := watchWait(t)
	_ = first.Close()
	recvClosed(t, reached, "the first window's hold to end")
	second := w.client(t)
	got := make(chan error, 1)
	go func() { _, err := second.Window(context.Background(), "grant-2", ""); got <- err }()
	recvClosed(t, waiting, "the reopen to wait for the slot")
	select {
	case err := <-got:
		t.Fatalf("the reopen answered %v while the first window still held its slot", err)
	default:
	}
	let()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("the reopen after the release = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reopen never answered")
	}
	if w.scr.opens() != 2 || w.scr.mostOpen() != 1 {
		t.Fatalf("%d windows opened, at most %d at once", w.scr.opens(), w.scr.mostOpen())
	}
}

// watchWait signals each time an open starts waiting for an ending window's slot.
func watchWait(t *testing.T) <-chan struct{} {
	t.Helper()
	ch := make(chan struct{}, 8)
	t.Cleanup(macvm.SetWindowWaitHook(func() {
		select {
		case ch <- struct{}{}:
		default:
		}
	}))
	return ch
}

// An opener that leaves while it waits gets no window: the slot is given back and the next open takes it.
func TestWindowReopenThatLeavesWhileWaitingOpensNothing(t *testing.T) {
	w := startWindowRun(t, true, nil)
	t.Cleanup(macvm.SetWindowReleaseWait(time.Minute))
	reached, let := holdRelease(t)
	waiting := watchWait(t)
	first := w.client(t)
	if _, err := first.Window(context.Background(), "grant-1", "first"); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	recvClosed(t, reached, "the first window's hold to end")
	second := w.client(t)
	got := make(chan error, 1)
	go func() { _, err := second.Window(context.Background(), "grant-2", "second"); got <- err }()
	recvClosed(t, waiting, "the reopen to wait for the slot")
	_ = second.Close()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the closed opener's call never returned")
	}
	checked := make(chan struct{})
	var once sync.Once
	t.Cleanup(macvm.SetWindowWaitedHook(func() { once.Do(func() { close(checked) }) }))
	let()
	// The leaver has checked its connection after the wait before the next open is sent, so the next open
	// meets a free slot or one the leaver still holds, never a window the leaver made.
	recvClosed(t, checked, "the leaver to check its connection after the wait")
	if _, err := w.client(t).Window(context.Background(), "grant-3", "third"); err != nil {
		t.Fatalf("an open after the leaver = %v", err)
	}
	if n := w.scr.opens(); n != 2 || w.scr.texts[1].Title != "third" || w.scr.mostOpen() != 1 {
		t.Fatalf("%d windows opened (%+v), at most %d at once", n, w.scr.texts, w.scr.mostOpen())
	}
}

// The wait for an ending window is bounded: past it, the reopen is refused as already open.
func TestWindowReopenWaitIsBounded(t *testing.T) {
	w := startWindowRun(t, true, nil)
	t.Cleanup(macvm.SetWindowReleaseWait(100 * time.Millisecond))
	reached, let := holdRelease(t)
	first := w.client(t)
	if _, err := first.Window(context.Background(), "grant-1", ""); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	recvClosed(t, reached, "the first window's hold to end")
	_, err := w.client(t).Window(context.Background(), "grant-2", "")
	wantRefusal(t, err, macvm.WindowCodeOpen, macvm.WindowAlreadyOpenSentence)
	let()
	if w.scr.opens() != 1 {
		t.Fatalf("%d windows opened", w.scr.opens())
	}
}

func recvClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestWindowRefusedWhenTheConsoleCheckFails(t *testing.T) {
	for name, tc := range map[string]struct {
		err      error
		sentence string
	}{
		"locked or not the console": {fmt.Errorf("locked: %w", macvm.ErrNotAtMac), macvm.WindowNotAtMacSentence},
		"unreadable":                {errors.New("no answer"), macvm.WindowUnreadableSentence},
		"lock unknown":              {fmt.Errorf("no lock key: %w", macvm.ErrLockUnknown), macvm.WindowLockUnknownSentence},
	} {
		t.Run(name, func(t *testing.T) {
			w := startWindowRun(t, true, func(_ *windowEngine, s *fakeScreen) { s.readyErr = tc.err })
			_, err := w.client(t).Window(context.Background(), "grant-1", "")
			code := macvm.WindowCodeNotAtMac
			if errors.Is(tc.err, macvm.ErrLockUnknown) {
				code = macvm.WindowCodeLockUnknown
			}
			wantRefusal(t, err, code, tc.sentence)
			if w.scr.opens() != 0 {
				t.Fatal("a window opened with the console check failing")
			}
			// The refusal freed the slot: with the check passing, the window opens.
			w.scr.setReady(nil)
			if _, err := w.client(t).Window(context.Background(), "grant-2", ""); err != nil {
				t.Fatalf("after the check passes: %v", err)
			}
		})
	}
}

func TestWindowClosesWhenTheScreenLocks(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		reason string
	}{
		"locked":             {macvm.ErrNotAtMac, macvm.WindowClosedHostLocked},
		"lock turns unknown": {fmt.Errorf("no lock key: %w", macvm.ErrLockUnknown), macvm.WindowClosedHostLocked},
		"unreadable":         {errors.New("no answer"), macvm.WindowClosedFailed},
	} {
		t.Run(name, func(t *testing.T) {
			w := startWindowRun(t, true, nil)
			ended, err := w.client(t).Window(context.Background(), "grant-1", "")
			if err != nil {
				t.Fatal(err)
			}
			w.scr.setReady(tc.err)
			if got := recv(t, ended); got != tc.reason {
				t.Fatalf("reason %q, want %q", got, tc.reason)
			}
			if w.scr.win(0).closes() == 0 {
				t.Fatal("the window stayed up")
			}
			waitState(t, w.cc, macvm.StateRunning)
		})
	}
}

// A lock notification closes an open window at once as the Mac locking, though the console check still passes.
// The window does not come back when the screen unlocks: a reopen is a new request, with its own grant, that the
// console check answers again.
func TestWindowLockNoticeClosesAndReopenNeedsANewGrant(t *testing.T) {
	w := startWindowRun(t, true, nil)
	evs, err := w.client(t).Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ended, err := w.client(t).Window(context.Background(), "grant-1", "")
	if err != nil {
		t.Fatal(err)
	}
	w.scr.win(0).lockNotice()
	if got := recv(t, ended); got != macvm.WindowClosedHostLocked {
		t.Fatalf("reason %q, want %q", got, macvm.WindowClosedHostLocked)
	}
	waitWindow(t, "the window to close", func() bool { return w.scr.win(0).closes() > 0 })
	waitState(t, w.cc, macvm.StateRunning)
	// The console check passes again, and still nothing opens by itself.
	time.Sleep(100 * time.Millisecond)
	if w.scr.opens() != 1 {
		t.Fatalf("%d windows opened after the lock, want none", w.scr.opens()-1)
	}
	// A reopen while the screen's lock can't be told is refused; once it can, the new grant opens.
	w.scr.setReady(macvm.ErrLockUnknown)
	_, err = w.client(t).Window(context.Background(), "grant-2", "")
	wantRefusal(t, err, macvm.WindowCodeLockUnknown, macvm.WindowLockUnknownSentence)
	w.scr.setReady(nil)
	if _, err := w.client(t).Window(context.Background(), "grant-3", ""); err != nil {
		t.Fatal(err)
	}
	var opened []string
	for len(opened) < 2 {
		select {
		case ev := <-evs:
			if ev.Type == macvm.EventWindow && ev.Detail == macvm.EventWindowOpen {
				opened = append(opened, ev.Grant)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("window opens seen: %v", opened)
		}
	}
	if opened[0] != "grant-1" || opened[1] != "grant-3" {
		t.Fatalf("window opens %v, want grant-1 then grant-3", opened)
	}
}

func TestWindowEndsWhenTheRunEnds(t *testing.T) {
	w := startWindowRun(t, true, nil)
	ended, err := w.client(t).Window(context.Background(), "grant-1", "")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = w.client(t).Stop(context.Background(), false, time.Second) }()
	if got := recv(t, ended); got != macvm.WindowClosedRunEnded && got != macvm.WindowClosedConnection {
		t.Fatalf("reason %q", got)
	}
	waitWindow(t, "the window to close", func() bool { return w.scr.win(0).closes() > 0 })
}

func TestWindowRefusedWhenTheVMPointerIsUnreadable(t *testing.T) {
	w := startWindowRun(t, true, func(e *windowEngine, _ *fakeScreen) { e.vmErr = errors.New("layout changed") })
	_, err := w.client(t).Window(context.Background(), "grant-1", "")
	wantRefusal(t, err, macvm.WindowCodeUnavailable, macvm.WindowUnavailableSentence)
	if w.scr.opens() != 0 {
		t.Fatal("a window opened without the VM's pointer")
	}
	waitState(t, w.cc, macvm.StateRunning)
}

func TestWindowRefusedWhenItCannotBeMade(t *testing.T) {
	w := startWindowRun(t, true, func(_ *windowEngine, s *fakeScreen) { s.openErr = errors.New("no display") })
	_, err := w.client(t).Window(context.Background(), "grant-1", "")
	wantRefusal(t, err, macvm.WindowCodeFailed, macvm.WindowFailedSentence+"no display")
}

// A lock that lands after the caller's console check but before the window goes up is caught by the
// check Open runs again, and refused as the lock it is, not as a failed window.
func TestWindowLockDuringOpenIsRefusedAsALock(t *testing.T) {
	for _, tc := range []struct {
		err      error
		code     string
		sentence string
	}{
		{fmt.Errorf("check: %w", macvm.ErrNotAtMac), macvm.WindowCodeNotAtMac, macvm.WindowNotAtMacSentence},
		{fmt.Errorf("check: %w", macvm.ErrLockUnknown), macvm.WindowCodeLockUnknown, macvm.WindowLockUnknownSentence},
		{fmt.Errorf("check: %w", macvm.ErrConsoleUnreadable), macvm.WindowCodeNotAtMac, macvm.WindowUnreadableSentence},
	} {
		w := startWindowRun(t, true, func(_ *windowEngine, s *fakeScreen) { s.openErr = tc.err })
		_, err := w.client(t).Window(context.Background(), "grant-1", "")
		wantRefusal(t, err, tc.code, tc.sentence)
		if w.scr.opens() != 0 {
			t.Fatalf("%d windows opened, want none", w.scr.opens())
		}
	}
}

func TestWindowNeedsAGrant(t *testing.T) {
	w := startWindowRun(t, true, nil)
	for _, grant := range []string{"", "has space", "semi;colon", string(make([]byte, 129))} {
		_, err := w.client(t).Window(context.Background(), grant, "")
		wantRefusal(t, err, macvm.WindowCodeBadRequest, macvm.WindowBadRequestSentence)
	}
	if w.scr.opens() != 0 {
		t.Fatal("a window opened with no usable grant")
	}
}

func TestWindowRefusedOnAMachineWithoutAView(t *testing.T) {
	if macvm.HasNativeWindowScreen() {
		t.Skip("the Mac's own screen is built in")
	}
	eng := &macvmtest.Engine{}
	r := startRun(t, eng, nil) // no Window, and the fake machine has no ViewHandle
	r.attach(t)
	waitState(t, r.cc, macvm.StateRunning)
	cc, err := macvm.DialControl(context.Background(), filepath.Join(r.sock, macvm.ControlSock))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	_, err = cc.Window(context.Background(), "grant-1", "")
	wantRefusal(t, err, macvm.WindowCodeUnavailable, macvm.WindowUnavailableSentence)
}

// A window adds no device: the VM's plan is built once, before it starts, and a window neither rebuilds nor
// changes it. This is the whole of the check without a Mac; the NIC and proxy stay plan.go's.
func TestWindowLeavesTheVMConfigurationAlone(t *testing.T) {
	w := startWindowRun(t, true, nil)
	m := w.eng.Machines()[0]
	before := fmt.Sprintf("%+v", m.Plan)
	if err := macvm.CheckPlan(m.Plan); err != nil {
		t.Fatal(err)
	}
	ended, err := w.client(t).Window(context.Background(), "grant-1", "")
	if err != nil {
		t.Fatal(err)
	}
	w.scr.win(0).byPerson()
	recv(t, ended)
	if after := fmt.Sprintf("%+v", m.Plan); after != before {
		t.Fatalf("the plan changed across a window:\nbefore %s\nafter  %s", before, after)
	}
	if err := macvm.CheckPlan(m.Plan); err != nil {
		t.Fatalf("the plan no longer checks after a window: %v", err)
	}
	if w.eng.built != 1 || len(w.eng.Machines()) != 1 {
		t.Fatalf("a window built %d machines (%d recorded); want the run's one", w.eng.built, len(w.eng.Machines()))
	}
}
