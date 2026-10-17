// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
// epoch it was admitted under and the bytes its final decision may take.
type spoolReservation struct {
	epoch uuid.UUID
	bytes int64
}

// spoolRecordEnvelope is what a decision record holds beyond its request's own
// strings: identifiers, times, the rule source, a cause sentence and a scan
// summary without findings (a skip or bypass).
const spoolRecordEnvelope = 64 << 10

// spoolAdmitTimeout bounds how long a request waits for its audit reservation before it is refused.
const spoolAdmitTimeout = 5 * time.Second

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
func (s *decisionSink) reserve(ctx context.Context, need int64) (uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.localErr != nil {
		return uuid.Nil, errors.New("runner audit spool unavailable")
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
		return uuid.Nil, errors.Join(err, s.refuse(ctx))
	}
	id := uuid.New()
	s.reservations[id] = spoolReservation{epoch: runneractivity.Capture(s.activityDir), bytes: need}
	s.heldBytes += need
	return id, nil
}

// refuse counts one refused admission, whatever refused it, for the builtin:spool-full record. The
// count runs on its own deadline: a request refused because its admission deadline ran out behind
// other requests' fsyncs is a refusal too, and that deadline has already passed. The caller holds s.mu.
func (s *decisionSink) refuse(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), spoolAdmitTimeout)
	defer cancel()
	return s.local.Refuse(ctx)
}

// drop ends a reservation; the caller holds s.mu.
func (s *decisionSink) drop(id uuid.UUID) {
	s.heldBytes -= s.reservations[id].bytes
	delete(s.reservations, id)
}

func (s *decisionSink) release(id uuid.UUID) { s.mu.Lock(); s.drop(id); s.mu.Unlock() }

// admitSpoolHTTP reserves r's audit record before r is served. mitmHost is the
// terminated tunnel's host for an inner request, "" for one ServeHTTP admits.
func (p *Proxy) admitSpoolHTTP(w http.ResponseWriter, r *http.Request, mitmHost string) (*http.Request, func()) {
	if p.sink == nil || p.sink.local == nil {
		return r, func() {}
	}
	// JSON can expand one control byte into six. Refuse an oversized target
	// before forwarding rather than discover that the final row cannot fit.
	if r.URL == nil || 6*(len(r.URL.String())+len(r.Host)+len(r.Method)) > runnerspool.MaxRecordBytes-4096 {
		p.refuseUnaudited(w, r, mitmHost, http.StatusRequestURITooLong, reasonAuditRecordTooLarge, "egress audit record too large",
			"the request target is too long to record in the run's egress audit, so it was not sent")
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), spoolAdmitTimeout)
	defer cancel()
	id, err := p.sink.reserve(ctx, p.spoolReservationBytes(r))
	if err != nil {
		p.refuseUnaudited(w, r, mitmHost, http.StatusServiceUnavailable, reasonAuditUnavailable, "egress audit spool unavailable",
			"the run's egress audit cannot record another request now, so it was not sent")
		return nil, nil
	}
	return r.WithContext(context.WithValue(r.Context(), spoolTicketKey{}, id)), func() { p.sink.release(id) }
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
	held, admitted := s.reservations[id]
	epoch := held.epoch
	admitted = admitted && log.Decision != egress.Pending
	b, err := json.Marshal(log)
	if err == nil {
		b = maskDecisionBytes(b)
		err = json.Unmarshal(b, &log)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err == nil && admitted {
		s.drop(id)
		err = s.local.AppendAdmitted(ctx, log)
	} else if err == nil {
		depth, bytes, _, statErr := s.local.Stats(ctx)
		if statErr != nil {
			err = statErr
		} else if depth+len(s.reservations) >= runnerspool.MaxRecords || bytes+s.heldBytes+int64(len(b))+1024 > runnerspool.MaxBytes {
			err = errors.Join(runnerspool.ErrFull, s.local.Refuse(ctx))
		} else {
			err = s.local.Append(ctx, log)
		}
	}
	if err == nil && admitted && log.RuleSource != ruleSourceApprovals && log.RuleSource != "credential:reauth-timeout" {
		_ = runneractivity.Record(s.activityDir, epoch)
	}
	if err != nil && !errors.Is(err, runnerspool.ErrFull) {
		s.localErr = err
	}
}

func configuredDecisionSink(cfg *Config, token *tokenSource, client *http.Client, out io.Writer) (*decisionSink, error) {
	if cfg.DecisionSpoolDir != "" {
		return newLocalDecisionSink(cfg.DecisionSpoolDir, cfg.RunID, out)
	}
	return newDecisionSink(cfg.ControlPlaneURL, token, cfg.DecisionBufferSize, client, out), nil
}
