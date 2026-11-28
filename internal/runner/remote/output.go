// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

type outputSink struct {
	mu     sync.Mutex
	writer io.Writer
	offset int64
	end    func(error)
	ended  bool
	// pending: an adopted sink before RecoverOutput has set its offset and gap; recovering: while its
	// recovery stream catches up. HandleOpen turns the runner's OPEN away with a capacity reset while
	// pending, and while recovering from an offset the sink has not reached, so the runner retries.
	pending, recovering bool
	// attached is the streams whose receiveOutput copies run into the sink; ref is the sandbox the sink is for, set
	// once a create's reply or an adoption names it. A sink with a ref is under the sink check (checkSinks):
	// idleSince is when it was last seen with nothing attached while the runner read online, stallRecovering marks
	// a stall recovery in flight, and dropped is a sink that yielded and accepts no byte more.
	attached        map[*runnerwire.Stream]struct{}
	ref             string
	idleSince       time.Time
	stallRecovering bool
	dropped         bool
}

func (s *outputSink) accept(offset int64, data []byte) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropped {
		return s.offset, errOutputYielded
	}
	if offset < 0 || offset > s.offset {
		return s.offset, runner.ErrOutputUnrecoverable
	}
	skip := min(int64(len(data)), s.offset-offset)
	data = data[skip:]
	if len(data) == 0 {
		return s.offset, nil
	}
	if s.ended {
		return s.offset, io.ErrClosedPipe
	}
	if s.end == nil {
		s.end = runner.BeginOutputDrain(s.writer)
	}
	n, err := s.writer.Write(data)
	if n < 0 || n > len(data) {
		return s.offset, errors.New("remote: invalid output writer count")
	}
	s.offset += int64(n)
	if n < len(data) && err == nil {
		err = io.ErrShortWrite
	}
	return s.offset, err
}

func (s *outputSink) finish(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	s.ended = true
	if s.end == nil {
		s.end = runner.BeginOutputDrain(s.writer)
	}
	s.end(err)
}

// HandleOpen serves the two streams a runner opens: a relay (handleRelay) and
// an output copy, which resumes at a raw byte offset. Duplicate prefixes are
// discarded; a forward gap is an error. ACK follows successful writer acceptance.
// An output open with no sink here goes to the run's launcher under high
// availability (forwardOpen), and is refused otherwise.
func (s *Substrate) HandleOpen(st *runnerwire.Stream, o runnerwire.Open) error {
	if o.Kind == runnerwire.KindRelay {
		return s.handleRelay(st, o)
	}
	return s.openOutput(st, o, true)
}

// ServeForwardedOutput answers an output open another replica forwarded to this one, the run's launcher, against
// this replica's own sink only: it never forwards again.
func (s *Substrate) ServeForwardedOutput(st *runnerwire.Stream, o runnerwire.Open) error {
	return s.openOutput(st, o, false)
}

func (s *Substrate) openOutput(st *runnerwire.Stream, o runnerwire.Open, forward bool) error {
	if o.Kind != runnerwire.KindOutput || o.Offset < 0 {
		return ErrUnsupportedStream
	}
	id, err := uuid.Parse(o.Target)
	if err != nil || id == uuid.Nil {
		return ErrUnsupportedStream
	}
	s.mu.Lock()
	sink := s.outputs[id]
	if f := s.forwards[id]; sink != nil && f != nil {
		f.waitSince = time.Time{} // a local sink ends a wait for the launcher
	}
	s.mu.Unlock()
	if sink == nil {
		if l, pt := s.outputForwarder(); forward && l != nil {
			return s.forwardOpen(l, pt, st, id, o)
		}
		return ErrUnsupportedStream
	}
	sink.mu.Lock()
	wait := sink.pending || (sink.recovering && o.Offset > sink.offset)
	sink.mu.Unlock()
	if wait {
		return runnerwire.ErrOpenCapacity // the runner retries a capacity reset
	}
	go func() { _ = s.receiveOutput(id, sink, st, o.Offset) }()
	return nil
}

func (s *Substrate) outputACK(ctx context.Context, id uuid.UUID, offset int64) error {
	delay := 50 * time.Millisecond
	for {
		attempt, cancel := context.WithTimeout(ctx, callTimeout)
		err := s.call(attempt, runnerwire.MethodOutputAck, runnerwire.OutputAckArgs{RunID: id, Offset: offset}, nil)
		cancel()
		if err == nil || ctx.Err() != nil {
			return err
		}
		// Only transport ambiguity is retryable. A runner refusal or disk error
		// must finish the recording with failure, never spin on a stopped buffer.
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, runner.ErrRunnerOffline) {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay = min(2*delay, time.Second)
	}
}

// outputAccepted commits accepted through Options.OutputAccepted before the
// ACK, retrying a failure with outputACK's backoff for up to callTimeout.
func (s *Substrate) outputAccepted(ctx context.Context, id uuid.UUID, accepted int64) error {
	if s.opts.OutputAccepted == nil {
		return nil
	}
	delay := 50 * time.Millisecond
	deadline := time.Now().Add(callTimeout)
	for {
		err := s.opts.OutputAccepted(ctx, id, accepted)
		if err == nil || ctx.Err() != nil || time.Now().After(deadline) {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay = min(2*delay, time.Second)
	}
}

// receiveOutput returns nil at EOF and otherwise the error that ended it.
func (s *Substrate) receiveOutput(id uuid.UUID, sink *outputSink, st *runnerwire.Stream, offset int64) error {
	// Output has no stdin. Half-close it so an EOF can release both wire ends
	// without RESET, which would discard bytes still waiting in a reader.
	_ = st.CloseWrite()
	sink.attach(st)
	defer sink.detach(st)
	data := make([]byte, 64<<10)
	for {
		n, readErr := st.Read(data)
		accepted, writeErr := sink.accept(offset, data[:n])
		offset += int64(n)
		if errors.Is(writeErr, errOutputYielded) {
			// The sink yielded to the process that took the run over: this one records and ACKs nothing more.
			_ = st.Close()
			return writeErr
		}
		// The org's record of what it accepted is committed before the runner
		// is told. Failing that, the stream ends as a transport loss does, with
		// no ACK: the runner keeps the bytes and the sink keeps its writer.
		if err := s.outputAccepted(st.Context(), id, accepted); err != nil {
			_ = st.Close()
			return err
		}
		// Even a short write has accepted its prefix; only those bytes are ACKed.
		ackErr := s.outputACK(st.Context(), id, accepted)
		if writeErr != nil {
			sink.finish(writeErr)
			_ = st.Close()
			return writeErr
		}
		if ackErr != nil {
			if st.Context().Err() == nil {
				sink.finish(ackErr)
			} else if reset := st.Err(); reset != nil && errors.Is(ackErr, context.Canceled) {
				ackErr = reset // the ACK saw only the stream's end: report why it ended
			}
			_ = st.Close()
			return ackErr
		} // transport loss retains writer and offset for reconnect
		if errors.Is(readErr, io.EOF) {
			sink.finish(nil)
			s.mu.Lock()
			if s.outputs[id] == sink {
				delete(s.outputs, id)
			}
			s.mu.Unlock()
			return nil
		}
		if readErr != nil {
			return readErr
		} // transport loss preserves accepted offset and drain
	}
}

// LauncherState is what a read of a run's launcher found for its output.
type LauncherState int

const (
	// LauncherNone: no such run, or it ended or was lost; its output is refused.
	LauncherNone LauncherState = iota
	// LauncherWait: the lease or its replica row is stale; the open is answered as capacity for now.
	LauncherWait
	// LauncherFresh: a fresh lease whose process has a fresh replica row, ReplicaID at PeerURL.
	LauncherFresh
)

// OutputLauncher is the replica whose process holds a run's watcher lease, where the run's output sink is.
type OutputLauncher struct {
	State     LauncherState
	ReplicaID string
	PeerURL   string
}

// The bounds of a forwarded output open and of the sink check. Variables only so a test can shorten them.
var (
	// forwardAttemptTimeout bounds one attempt: the launcher read, the dial and the forwarded OPEN.
	forwardAttemptTimeout = 5 * time.Second
	// forwardReadyFor is how long a stream the launcher accepted waits for the runner's retry to take it.
	forwardReadyFor = 10 * time.Second
	// forwardRefusedFor is how long a refusal answers the run's opens here without a new attempt.
	forwardRefusedFor = 10 * time.Second
	// forwardWaitCeiling is the longest a run's opens are answered capacity while its launcher reads LauncherWait:
	// the holder's claim comes within 150 s of the launcher's last heartbeat (90 s stale, then a 60 s sweep), plus
	// one 30 s heartbeat interval; it equals outputAckWait, the longest a full buffer parks the agent anyway.
	forwardWaitCeiling = 180 * time.Second
	// outputSinkCheck is the sink check's period when Options.OutputSinkCheck is zero.
	outputSinkCheck = 10 * time.Second
	// outputStallAfter is how long a launcher's sink may have nothing attached, while its runner reads online,
	// before the launcher recovers the output itself.
	outputStallAfter = 30 * time.Second
)

// errOutputYielded finishes a sink that yielded to the process holding its run's watcher lease.
var errOutputYielded = errors.New("remote: the run's output went to the process that holds its watcher lease")

// outputForward is one run's sink-less output opens on their way to its launcher, under Substrate.mu.
type outputForward struct {
	attempting bool          // an attempt is running
	ready      *readyForward // a stream the launcher accepted, for the runner's next retry
	refusedAt  time.Time     // the last refusal; zero for none
	waitSince  time.Time     // the first LauncherWait read since the last other answer; zero for none
	touched    time.Time     // the last open
}

// readyForward is a stream the launcher accepted for an open at offset, over its own peer WebSocket.
type readyForward struct {
	offset int64
	st     *runnerwire.Stream
	peer   *runnerwire.Peer
}

func (r *readyForward) drop() {
	if r == nil {
		return
	}
	r.st.Abort(runnerwire.ResetCancelled)
	r.peer.Close()
}

// outputForwarder is this Substrate's Link and peer client when a sink-less output open may be forwarded: a Link
// routed through the holder view, with the launcher read set. Both are nil otherwise.
func (s *Substrate) outputForwarder() (*Link, *PeerTransport) {
	l, ok := s.t.(*Link)
	if !ok || s.opts.RunOutputLauncher == nil {
		return nil, nil
	}
	if pt := l.peerTransport(); pt != nil {
		return l, pt
	}
	return nil, nil
}

// forwardOpen answers, at once, a runner's output open for run id that found no sink here. It never waits: it runs
// on the session's read loop. A ready forward at the open's offset is taken and copied; a refusal in the last
// forwardRefusedFor refuses; otherwise an attempt starts, unless one is running, and the open is answered capacity
// so the runner retries at the same offset.
func (s *Substrate) forwardOpen(l *Link, pt *PeerTransport, st *runnerwire.Stream, id uuid.UUID, o runnerwire.Open) error {
	now := time.Now()
	s.mu.Lock()
	for rid, f := range s.forwards {
		if !f.attempting && f.ready == nil && now.Sub(f.touched) > forwardWaitCeiling {
			delete(s.forwards, rid)
		}
	}
	f := s.forwards[id]
	if f == nil {
		f = &outputForward{}
		s.forwards[id] = f
	}
	f.touched = now
	r := f.ready
	f.ready = nil
	if r != nil && r.offset == o.Offset {
		s.mu.Unlock()
		go copyForward(st, r)
		return nil
	}
	refused := !f.refusedAt.IsZero() && now.Sub(f.refusedAt) < forwardRefusedFor
	if !refused && !f.attempting {
		f.attempting = true
		go s.forwardAttempt(l, pt, id, o)
	}
	s.mu.Unlock()
	r.drop() // a stream opened at another offset than this open's
	if refused {
		return ErrUnsupportedStream
	}
	return runnerwire.ErrOpenCapacity
}

// forwardAttempt runs one attempt for run id under forwardAttemptTimeout and records what it found: a ready
// stream, a refusal, or nothing, so the next retry starts a new attempt.
func (s *Substrate) forwardAttempt(l *Link, pt *PeerTransport, id uuid.UUID, o runnerwire.Open) {
	ctx, cancel := context.WithTimeout(context.Background(), forwardAttemptTimeout)
	defer cancel()
	ready, refused := s.tryForward(ctx, l, pt, id, o)
	s.mu.Lock()
	f := s.forwards[id]
	if f == nil {
		f = &outputForward{touched: time.Now()}
		s.forwards[id] = f
	}
	f.attempting = false
	if refused {
		f.refusedAt = time.Now()
	}
	stale := f.ready
	if ready != nil {
		f.ready = ready
		time.AfterFunc(forwardReadyFor, func() { s.dropReady(id, ready) })
	} else {
		stale = nil
	}
	s.mu.Unlock()
	stale.drop()
}

// dropReady resets r if no retry took it.
func (s *Substrate) dropReady(id uuid.UUID, r *readyForward) {
	s.mu.Lock()
	f := s.forwards[id]
	mine := f != nil && f.ready == r
	if mine {
		f.ready = nil
	}
	s.mu.Unlock()
	if mine {
		r.drop()
	}
}

// waitFor records one launcher read for run id: a LauncherWait starts the wait clock or reports it past
// forwardWaitCeiling; any other answer clears it.
func (s *Substrate) waitFor(id uuid.UUID, wait bool) (over bool) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.forwards[id]
	if f == nil {
		return false
	}
	switch {
	case !wait:
		f.waitSince = time.Time{}
	case f.waitSince.IsZero():
		f.waitSince = now
	case now.Sub(f.waitSince) >= forwardWaitCeiling:
		f.waitSince = time.Time{} // answered as LauncherNone
		return true
	}
	return false
}

// tryForward reads the run's launcher and, when it is another replica, opens the runner's output there on this
// replica's session. It answers the launcher's accepted stream, or whether the open is refused for now.
func (s *Substrate) tryForward(ctx context.Context, l *Link, pt *PeerTransport, id uuid.UUID, o runnerwire.Open) (*readyForward, bool) {
	at, err := s.opts.RunOutputLauncher(ctx, id)
	if err != nil {
		return nil, false
	}
	switch at.State {
	case LauncherNone:
		s.waitFor(id, false)
		return nil, true
	case LauncherWait:
		return nil, s.waitFor(id, true)
	}
	s.waitFor(id, false)
	if at.ReplicaID == pt.Self() {
		return nil, true // the launcher is this replica, and it has no sink for the run
	}
	l.mu.Lock()
	runnerID := l.runner
	l.mu.Unlock()
	// HeldHere, not Online: the open is forwarded from this replica's own session, the one the view names here.
	route, ok := l.holder()
	if !ok || route.ReplicaID != pt.Self() || !l.HeldHere() {
		return nil, false
	}
	fp, err := pt.openOutput(ctx, runnerID, PeerRoute{ReplicaID: at.ReplicaID, PeerURL: at.PeerURL, SessionID: route.SessionID}, id)
	if err != nil {
		re := (*peerRefusedError)(nil)
		return nil, errors.As(err, &re) && re.class == "no_sink"
	}
	fst, err := fp.Open(ctx, runnerwire.Open{Kind: runnerwire.KindOutput, Target: o.Target, Offset: o.Offset})
	if err != nil {
		fp.Close()
		re := (*runnerwire.ResetError)(nil)
		return nil, errors.As(err, &re) && re.Code == runnerwire.ResetRefused
	}
	return &readyForward{offset: o.Offset, st: fst, peer: fp}, false
}

// copyForward copies the runner's output stream to the launcher's and passes an end or a reset each way: the
// launcher half-closes its side at once, as receiveOutput does, and the runner's EOF half-closes the launcher's.
// The launcher's peer WebSocket ends with the copy.
func copyForward(runnerSt *runnerwire.Stream, r *readyForward) {
	defer r.peer.Close()
	go peerCopy(runnerSt, r.st)
	go func() {
		select {
		case <-r.st.Done():
			if re := (*runnerwire.ResetError)(nil); errors.As(r.st.Err(), &re) {
				runnerSt.Abort(re.Code)
			}
		case <-runnerSt.Done():
			if re := (*runnerwire.ResetError)(nil); errors.As(runnerSt.Err(), &re) {
				r.st.Abort(re.Code)
			}
		}
	}()
	if _, err := io.Copy(r.st, runnerSt); err != nil {
		runnerSt.Abort(runnerwire.ResetCancelled)
		r.st.Abort(runnerwire.ResetCancelled)
		return
	}
	_ = r.st.CloseWrite()
	t := time.NewTimer(forwardReadyFor)
	defer t.Stop()
	select {
	case <-r.st.Done():
	case <-r.peer.Done():
	case <-t.C:
	}
}

// ServeOutputDoor is the launcher's end of one output open the runner's holder forwarded here for run: the org
// end of a runnerwire peer over conn. Its first frame must be an OPEN of KindOutput naming run, answered against
// this replica's own sink (ServeForwardedOutput). A call, a second OPEN, or an OPEN of anything else is told
// "wrong_method" and closes the WebSocket, and no sink sees its bytes. The WebSocket closes once the stream it took
// ends.
func (s *Substrate) ServeOutputDoor(ctx context.Context, conn runnerwire.Conn, run uuid.UUID, refused func(class string)) error {
	var ws *runnerwire.Peer
	var first atomic.Bool
	wrong := func() {
		if refused != nil {
			refused("wrong_method")
		}
		go ws.Close()
	}
	onOpen := func(st *runnerwire.Stream, o runnerwire.Open) error {
		id, err := uuid.Parse(o.Target)
		if !first.CompareAndSwap(false, true) || o.Kind != runnerwire.KindOutput || err != nil || id != run {
			wrong()
			return ErrUnsupportedStream
		}
		if err := s.ServeForwardedOutput(st, o); err != nil {
			return err
		}
		go func() {
			select {
			case <-st.Done():
			case <-ws.Done():
			}
			ws.Close()
		}()
		return nil
	}
	onCall := func(context.Context, uint32, string, json.RawMessage) (any, error) {
		first.Store(true)
		wrong()
		return nil, errOffline
	}
	ws = runnerwire.NewPeer(conn, runnerwire.PeerConfig{Org: true, OnOpen: onOpen, OnCall: onCall})
	return ws.Run(ctx)
}

// ServableOutput is the launcher's door check for an output open of run forwarded here: this replica has the run's
// sink, and the lease rule reads sole, so no other process holds the run's watcher lease fresh. A sink the rule
// does not keep yields here, as at the sink check. A failed read serves nothing.
func (s *Substrate) ServableOutput(ctx context.Context, run uuid.UUID) bool {
	s.mu.Lock()
	sink := s.outputs[run]
	s.mu.Unlock()
	if sink == nil {
		return false
	}
	if s.opts.OutputLease == nil {
		return true
	}
	keep, sole, err := s.opts.OutputLease(ctx, run)
	if err != nil {
		return false
	}
	if !keep {
		s.yieldSink(run, sink)
		return false
	}
	return sole
}

func (sink *outputSink) attach(st *runnerwire.Stream) {
	sink.mu.Lock()
	if sink.attached == nil {
		sink.attached = map[*runnerwire.Stream]struct{}{}
	}
	sink.attached[st] = struct{}{}
	sink.mu.Unlock()
}

func (sink *outputSink) detach(st *runnerwire.Stream) {
	sink.mu.Lock()
	delete(sink.attached, st)
	sink.mu.Unlock()
}

// setSinkRef names the sandbox sink is for, which puts it under the sink check, and starts the check's loop if it
// is not running.
func (s *Substrate) setSinkRef(sink *outputSink, ref string) {
	if sink == nil {
		return
	}
	sink.mu.Lock()
	sink.ref = ref
	sink.mu.Unlock()
	if s.opts.OutputLease == nil {
		return
	}
	s.mu.Lock()
	start := !s.checking
	s.checking = true
	s.mu.Unlock()
	if start {
		go s.sinkLoop()
	}
}

// sinkLoop runs checkSinks every Options.OutputSinkCheck while any sink is under the check.
func (s *Substrate) sinkLoop() {
	every := s.opts.OutputSinkCheck
	if every <= 0 {
		every = outputSinkCheck
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for range t.C {
		if s.checkSinks(context.Background()) > 0 {
			continue
		}
		s.mu.Lock()
		if len(s.checkedSinksLocked()) == 0 { // a sink given its ref since the check read the map keeps the loop
			s.checking = false
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
	}
}

type checkedSink struct {
	id   uuid.UUID
	sink *outputSink
	ref  string
}

// checkedSinksLocked is every sink with a ref that has not ended, under s.mu.
func (s *Substrate) checkedSinksLocked() []checkedSink {
	var out []checkedSink
	for id, sink := range s.outputs {
		sink.mu.Lock()
		ref, ended := sink.ref, sink.ended
		sink.mu.Unlock()
		if ref != "" && !ended {
			out = append(out, checkedSink{id: id, sink: sink, ref: ref})
		}
	}
	return out
}

// checkSinks applies the lease rule to every sink under the check: one the rule does not keep yields. For a sink
// it keeps on a runner whose session this replica does not hold, it counts the time with nothing attached while the
// runner reads online, and recovers the output into the same sink past outputStallAfter, when no other process
// holds the run's lease fresh. A failed read keeps the sink and starts no recovery. It answers how many sinks it
// read.
func (s *Substrate) checkSinks(ctx context.Context) int {
	s.mu.Lock()
	sinks := s.checkedSinksLocked()
	s.mu.Unlock()
	// HeldHere, not Online, for the stall rule's scope: a sink whose session this replica holds is never stalled.
	// Online for its count: the runner reads online on any replica.
	held, online := true, s.t.Online()
	if l, ok := s.t.(interface{ HeldHere() bool }); ok {
		held = l.HeldHere()
	}
	for _, c := range sinks {
		lctx, cancel := context.WithTimeout(ctx, callTimeout)
		keep, sole, err := s.opts.OutputLease(lctx, c.id)
		cancel()
		if err != nil {
			continue
		}
		if !keep {
			s.yieldSink(c.id, c.sink)
			continue
		}
		if c.sink.stalled(held, online, sole, time.Now()) {
			go s.recoverStalled(c.id, c.sink, c.ref)
		}
	}
	return len(sinks)
}

// stalled counts the time the sink has had nothing attached while its runner read online on another replica's
// session; a check that finds it held here, offline, attached, or not yet set up resets the count. Past
// outputStallAfter, with no recovery in flight and sole set, it marks the sink recovering and reports true.
func (sink *outputSink) stalled(held, online, sole bool, now time.Time) bool {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if held || !online || len(sink.attached) > 0 || sink.pending || sink.ended || sink.dropped {
		sink.idleSince = time.Time{}
		return false
	}
	if sink.idleSince.IsZero() {
		sink.idleSince = now
		return false
	}
	if now.Sub(sink.idleSince) < outputStallAfter || sink.stallRecovering || !sole {
		return false
	}
	sink.stallRecovering = true
	return true
}

// recoverStalled reads the run's output back from the runner into its stalled sink, through this Substrate's pin,
// from the runner's start: a duplicate prefix is dropped, and a start past the sink's offset, or an output the
// runner cannot recover, ends the sink with runner.ErrOutputUnrecoverable. The sink keeps its own offset. A failed
// call is tried again at a later check.
func (s *Substrate) recoverStalled(id uuid.UUID, sink *outputSink, ref string) {
	defer func() {
		sink.mu.Lock()
		sink.stallRecovering, sink.recovering, sink.idleSince = false, false, time.Time{}
		sink.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	sess, err := s.pin(ctx)
	if err != nil {
		return
	}
	var res runnerwire.RecoverResult
	if err := sess.Call(ctx, runnerwire.MethodRecoverOutput, runnerwire.RecoverOutputArgs{Ref: ref, Gap: true}, &res); err != nil {
		return
	}
	if res.Unrecoverable {
		sink.finish(runner.ErrOutputUnrecoverable)
		return
	}
	st, err := sess.TakeStream(res.Stream)
	if err != nil {
		return
	}
	sink.mu.Lock()
	past := res.Gap < 0 || res.Gap > sink.offset
	if !past {
		sink.recovering = true
	}
	sink.mu.Unlock()
	if past {
		_ = st.Close()
		sink.finish(runner.ErrOutputUnrecoverable)
		return
	}
	_ = s.receiveOutput(id, sink, st, res.Gap)
}

// yieldSink drops a sink the lease rule no longer keeps: it leaves outputs, accepts no byte more, and every stream
// copying into it is reset refused. Then Options.OutputYielded runs, and only after it returns is the sink
// finished with errOutputYielded, which ends its writer's drain. A sink yields once.
func (s *Substrate) yieldSink(id uuid.UUID, sink *outputSink) {
	s.mu.Lock()
	if s.outputs[id] != sink {
		s.mu.Unlock()
		return
	}
	delete(s.outputs, id)
	s.mu.Unlock()
	sink.mu.Lock()
	sink.dropped = true
	attached := slices.Collect(maps.Keys(sink.attached))
	sink.mu.Unlock()
	for _, st := range attached {
		st.Abort(runnerwire.ResetRefused)
	}
	if s.opts.OutputYielded != nil {
		s.opts.OutputYielded(id)
	}
	sink.finish(errOutputYielded)
}
