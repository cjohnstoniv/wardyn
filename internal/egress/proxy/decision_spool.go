// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runneractivity"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

type spoolTicketKey struct{}

func spoolTicket(ctx context.Context) uuid.UUID {
	id, _ := ctx.Value(spoolTicketKey{}).(uuid.UUID)
	return id
}

func newLocalDecisionSink(dir string, id uuid.UUID, out io.Writer) (*decisionSink, error) {
	spool, err := runnerspool.Open(dir, id)
	if err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "writer.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		spool.Close()
		return nil, err
	}
	st, err := lock.Stat()
	link, linkErr := os.Lstat(filepath.Join(dir, "writer.lock"))
	if err != nil || linkErr != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || link.Mode()&os.ModeSymlink != 0 || !os.SameFile(st, link) {
		lock.Close()
		spool.Close()
		return nil, errors.New("unsafe proxy spool writer lock")
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		spool.Close()
		return nil, errors.New("another proxy owns this run spool")
	}
	if err := spool.CanRecord(context.Background()); err != nil && !errors.Is(err, runnerspool.ErrFull) {
		lock.Close()
		spool.Close()
		return nil, err
	}
	return &decisionSink{local: spool, writerLock: lock, out: out, activityDir: dir, reservations: map[uuid.UUID]spoolReservation{}}, nil
}

// spoolReservation is one admitted request's hold on the spool: the activity
// epoch it was admitted under and the bytes its records may still take. used
// is set once its first record has been charged.
type spoolReservation struct {
	epoch uuid.UUID
	bytes int64
	used  bool
}

// spoolFollowUpBytes is what a request keeps reserved after its first record,
// until release: room for the further records one served request may emit (a
// namespace-off allow, a credential heal and its replayed request).
const spoolFollowUpBytes = 16 << 10

// errAuditLost is a latched refusal: a served request's record did not land.
var errAuditLost = errors.New("runner audit spool unavailable")

// spoolRecordEnvelope is what a decision record holds beyond its request's own
// strings: identifiers, times, the rule source, a cause sentence and a scan
// summary without findings (a skip or bypass).
const spoolRecordEnvelope = 64 << 10

// spoolAdmitTimeout bounds how long a request waits for its audit reservation before it is refused,
// including the wait for room in a full spool.
const spoolAdmitTimeout = 5 * time.Second

// spoolRoomPoll is how often a request held for room rechecks a full spool. Room also appears when
// wardyn-runnerd's replay is acknowledged, which no in-process signal reports.
const spoolRoomPoll = 100 * time.Millisecond

// spoolAppendTimeout bounds how long a decision waits for the spool lock: the bound wardyn-runnerd's own
// spool calls run under (offline/manager.go replayLoop), so a lock held by a slow replay or reclaim
// delays a record rather than losing it.
const spoolAppendTimeout = 30 * time.Second

// spoolRetryAfter is the Retry-After, in seconds, on a refusal the audit could not record. The
// Anthropic and OpenAI SDKs retry a 5xx and honour a Retry-After of up to 60 s; X-Should-Retry: true
// tells them to retry whatever the status.
const spoolRetryAfter = "1"

// spoolReservationBytes is the most a request's final decision record may take.
// A request that may be scanned while content inspection is on (a body-bearing
// one, or one with a query when forward egress is inspected: an API door scans
// the query on any method) carries its findings in its record, up to the
// scanner's cap and each with a field path, which no request-derived bound
// covers: it reserves the largest wire record. Any other request reserves its
// target escaped twice over (JSON can expand one byte into six, and a cause
// sentence can name the target again) plus the record's envelope, never more
// than the largest wire record.
// Reserving every request at the wire maximum held a 32 MiB spool to 32
// requests in flight.
func (p *Proxy) spoolReservationBytes(r *http.Request) int64 {
	if p.scanner != nil && p.scanner.Mode() != contentscan.ModeOff &&
		(bodyBearingMethod(r.Method) || p.scanner.InspectForwardEgress() && r.URL.RawQuery != "") {
		return runnerspool.MaxRecordBytes
	}
	return min(int64(2*6*(len(r.URL.String())+len(r.Host)+len(r.Method))+spoolRecordEnvelope), runnerspool.MaxRecordBytes)
}

// Reserve the request's own record bound before forwarding. The mutex covers both
// reservation and append accounting; a burst cannot all observe the same free
// slot and then forward requests whose final decision has nowhere to land.
//
// A full spool holds the request until room appears or ctx ends; only then is
// the refusal counted. The spool's own bounds are the backlog a dropped runner
// link may build: admission never waits on the link itself.
func (s *decisionSink) reserve(ctx context.Context, need int64) (uuid.UUID, error) {
	if s.remote != nil {
		return s.remote.reserve(ctx, need)
	}
	sawFull := false
	for {
		if err := s.lockCtx(ctx); err != nil {
			return uuid.Nil, s.refused(ctx, sawFull, err)
		}
		id, err := s.tryReserve(ctx, need)
		s.mu.Unlock()
		if err == nil {
			return id, nil
		}
		// A deadline that runs out inside a check after the spool was seen full is still a full spool's refusal.
		if !errors.Is(err, runnerspool.ErrFull) {
			return uuid.Nil, s.refused(ctx, sawFull, err)
		}
		sawFull = true
		select {
		case <-ctx.Done():
		case <-time.After(spoolRoomPoll):
		}
		if ctx.Err() != nil {
			return uuid.Nil, s.refused(ctx, true, ctx.Err())
		}
	}
}

// tryReserve checks the spool once and reserves need when it has room; the caller holds s.mu.
func (s *decisionSink) tryReserve(ctx context.Context, need int64) (uuid.UUID, error) {
	if s.closed || s.localErr != nil {
		return uuid.Nil, errAuditLost
	}
	err := s.local.CanRecord(ctx)
	if err == nil {
		var depth int
		var bytes int64
		depth, bytes, _, err = s.local.Stats(ctx)
		if err == nil && (depth+len(s.reservations) >= runnerspool.MaxRecords || bytes+s.heldBytes+need > runnerspool.MaxBytes) {
			err = runnerspool.ErrFull
		}
	}
	if err != nil {
		return uuid.Nil, err
	}
	id := uuid.New()
	s.reservations[id] = spoolReservation{epoch: runneractivity.Capture(s.activityDir), bytes: need}
	s.heldBytes += need
	return id, nil
}

// refused ends an admission that did not reserve. A full spool's refusal (the spool was seen full and
// the admission deadline ran out) is counted; anything else is returned as it is.
func (s *decisionSink) refused(ctx context.Context, sawFull bool, err error) error {
	if !sawFull || !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, runnerspool.ErrFull) {
		return err
	}
	return errors.Join(runnerspool.ErrFull, s.refuse(ctx))
}

// lockCtx takes s.mu unless ctx ends first. A decision may hold s.mu for up to spoolAppendTimeout while
// it waits on the spool lock; admission must still answer within its own deadline.
func (s *decisionSink) lockCtx(ctx context.Context) error {
	for !s.mu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	return nil
}

// refuse counts one refused admission for the builtin:spool-full record. Only a spool that stayed full
// for the whole admission wait is counted: an admission that ran out of time behind other requests or
// whose client hung up gets an uncounted 503 instead. The count runs on its own deadline, since the
// request's own is spent.
func (s *decisionSink) refuse(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), spoolAdmitTimeout)
	defer cancel()
	if err := s.lockCtx(ctx); err != nil {
		return err
	}
	defer s.mu.Unlock()
	return s.local.Refuse(ctx)
}

// drop ends a reservation; the caller holds s.mu.
func (s *decisionSink) drop(id uuid.UUID) {
	s.heldBytes -= s.reservations[id].bytes
	delete(s.reservations, id)
}

func (s *decisionSink) release(id uuid.UUID) {
	if s.remote != nil {
		s.remote.release(id)
		return
	}
	s.mu.Lock()
	s.drop(id)
	s.mu.Unlock()
}

// admitSpoolHTTP reserves r's audit record before r is served. mitmHost is the
// terminated tunnel's host for an inner request, "" for one ServeHTTP admits.
func (p *Proxy) admitSpoolHTTP(w http.ResponseWriter, r *http.Request, mitmHost string) (*http.Request, func()) {
	// Also the gate for a terminated tunnel's inner requests: no credential is
	// injected once a self-lapsing proxy has closed.
	if !p.lapse.serving() {
		refuseClosed(w)
		return nil, nil
	}
	if p.sink == nil || (p.sink.local == nil && p.sink.remote == nil) {
		return r, func() {}
	}
	// JSON can expand one control byte into six. Refuse an oversized target
	// before forwarding rather than discover that the final row cannot fit.
	if r.URL == nil || 6*(len(r.URL.String())+len(r.Host)+len(r.Method)) > runnerspool.MaxRecordBytes-4096 {
		p.recordTooLarge(r, mitmHost)
		p.refuseUnaudited(w, r, mitmHost, http.StatusRequestURITooLong, reasonAuditRecordTooLarge, "egress audit record too large",
			"the request target is too long to record in the run's egress audit, so it was not sent")
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), spoolAdmitTimeout)
	defer cancel()
	id, err := p.sink.reserve(ctx, p.spoolReservationBytes(r))
	if err != nil {
		plain, sentence := "egress audit spool unavailable", "the run's egress audit is unavailable, so it was not sent"
		// Only a full spool or an admission that ran out of time can succeed on a retry; a sealed or
		// corrupt spool, or a lost record, refuses for the rest of the run.
		if errors.Is(err, runnerspool.ErrFull) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			sentence = "the run's egress audit cannot record another request now, so it was not sent"
			// wardyn-runnerd withdraws the activity epoch while its link to the organisation is down: a
			// full spool then cannot drain until the link is back.
			if errors.Is(err, runnerspool.ErrFull) && runneractivity.Capture(p.sink.activityDir) == uuid.Nil {
				plain += ": the runner's link to the organisation is down"
				sentence = "the runner's link to the organisation is down and the run's egress audit is full, so this request was not sent; retry"
			}
			w.Header().Set("Retry-After", spoolRetryAfter)
			w.Header().Set("X-Should-Retry", "true")
		}
		slog.Warn("proxy refused a request its egress audit could not record", "reason", reasonAuditUnavailable, "cause", sentence, "err", err)
		p.refuseUnaudited(w, r, mitmHost, http.StatusServiceUnavailable, reasonAuditUnavailable, plain, sentence)
		return nil, nil
	}
	return r.WithContext(context.WithValue(r.Context(), spoolTicketKey{}, id)), func() { p.sink.release(id) }
}

// ruleSourceAuditRecordTooLarge marks the deny written for a request whose target is too long to record.
const ruleSourceAuditRecordTooLarge = "builtin:audit-record-too-large"

// tooLargeFieldBytes bounds each request string the too-large deny keeps.
const tooLargeFieldBytes = 512

// recordTooLarge writes a bounded deny for a request refused because its target cannot be recorded:
// the host, method and the start of the path, never the query. It was never sent, so it needs no
// reservation; a spool with no room counts it as a refusal, as any unadmitted deny.
func (p *Proxy) recordTooLarge(r *http.Request, mitmHost string) {
	host, path := mitmHost, ""
	if r.URL != nil {
		path = r.URL.Path
		if host == "" {
			host = r.URL.Hostname()
		}
	}
	if host == "" {
		host = r.Host
	}
	clip := func(v string) string { return strings.ToValidUTF8(v[:min(len(v), tooLargeFieldBytes)], "") }
	p.sink.emit(decisionLog(egress.Request{RunID: p.runID, Host: clip(strings.ToLower(host)), Method: clip(strings.ToUpper(r.Method)),
		Path: clip(path), Time: p.now()}, egress.Deny, ruleSourceAuditRecordTooLarge))
}

// refuseUnaudited answers a request the audit could not record, keeping its
// status: plain text on the forward lane, the SDK's error shape on a brokered
// route or an LLM or AWS host. ServeHTTP admits before it routes, so a
// brokered route is told by its path here.
func (p *Proxy) refuseUnaudited(w http.ResponseWriter, r *http.Request, mitmHost string, status int, reason, plainMsg, sentence string) {
	shape := shapePlain
	switch {
	case mitmHost != "":
		shape = p.shapeOfHost(mitmHost)
	case r.Method == http.MethodConnect || r.URL == nil:
	case r.URL.Host != "":
		shape = p.shapeOfHost(r.URL.Hostname())
	case strings.HasPrefix(r.URL.Path, llmAnthropicPrefix):
		host, _, _ := p.llmUpstream(anthropicHost)
		shape = routeShape(shapeAnthropic, host)
	case strings.HasPrefix(r.URL.Path, llmOpenAIPrefix):
		host, _, _ := p.llmUpstream(openaiHost)
		shape = routeShape(shapeOpenAI, host)
	}
	if shape == shapePlain {
		http.Error(w, plainMsg, status)
		return
	}
	writeProviderError(w, shape, status, reason, "wardyn: "+reason+": "+sentence, nil)
}

func (s *decisionSink) emitLocal(log egress.DecisionLog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	id := log.Request.AuditReservation
	held, live := s.reservations[id]
	pending := log.Decision == egress.Pending
	// Served: an allowed request, or any final record of an admitted one. A deny or a pending hold
	// without a live reservation was never sent.
	served := !pending && (live || log.Decision == egress.Allow)
	b, err := json.Marshal(log)
	if err == nil {
		b = maskDecisionBytes(b)
		err = json.Unmarshal(b, &log)
	}
	ctx, cancel := context.WithTimeout(context.Background(), spoolAppendTimeout)
	defer cancel()
	size := int64(len(b)) + 1024
	charged := false
	if err == nil && live && !pending && (!held.used || size <= held.bytes) {
		// The first record takes its reservation (sized for it); a follow-up record takes what is left,
		// capped at spoolFollowUpBytes after the first. release drops the rest.
		keep := held.bytes - size
		if !held.used {
			keep = min(max(keep, 0), spoolFollowUpBytes)
		}
		s.heldBytes -= held.bytes - keep
		held.bytes, held.used = keep, true
		s.reservations[id] = held
		charged = true
		err = s.local.AppendAdmitted(ctx, log)
	} else if err == nil {
		depth, bytes, _, statErr := s.local.Stats(ctx)
		switch {
		case statErr != nil:
			err = statErr
		case depth+len(s.reservations) >= runnerspool.MaxRecords || bytes+s.heldBytes+size > runnerspool.MaxBytes:
			// A served record that does not fit is lost, not a refused admission: it is not counted.
			err = runnerspool.ErrFull
			if !served {
				err = errors.Join(err, s.local.Refuse(ctx))
			}
		default:
			err = s.local.Append(ctx, log)
		}
	}
	if err == nil && charged && log.RuleSource != ruleSourceApprovals && log.RuleSource != "credential:reauth-timeout" {
		_ = runneractivity.Record(s.activityDir, held.epoch)
	}
	// A served request whose record did not land, for any reason, is an audit gap: it refuses all
	// later egress. A deny or a pending hold was never sent, and a full or broken spool still refuses
	// at the next reservation.
	if err != nil && served {
		slog.Error("proxy lost the audit record of a served request; refusing further egress", "host", log.Request.Host, "rule_source", log.RuleSource, "err", err)
		s.localErr = err
	} else if err != nil && !errors.Is(err, runnerspool.ErrFull) {
		slog.Error("proxy could not spool a decision for a request it did not serve", "host", log.Request.Host, "rule_source", log.RuleSource, "err", err)
	}
}

func configuredDecisionSink(cfg *Config, token *tokenSource, client *http.Client, out io.Writer) (*decisionSink, error) {
	if cfg.DecisionSink != nil {
		remote, err := newRemoteSink(cfg.RunID, *cfg.DecisionSink)
		if err != nil {
			return nil, err
		}
		return &decisionSink{remote: remote, out: out}, nil
	}
	if cfg.DecisionSpoolDir != "" {
		return newLocalDecisionSink(cfg.DecisionSpoolDir, cfg.RunID, out)
	}
	return newDecisionSink(cfg.ControlPlaneURL, token, cfg.DecisionBufferSize, client, out), nil
}
