// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// The decision door's batch wire (#4063). A control plane that takes batches says so on every answer of its
// decision door; a proxy posts a batch only after reading that, and never infers it from a status.
const (
	DecisionBatchContentType = "application/vnd.wardyn.decision-batch+json"
	// DecisionBatchHeader is on every answer of a control plane's decision door that takes batches; its value
	// is the most records one batch may carry.
	DecisionBatchHeader = "Wardyn-Decision-Batch"
	// DecisionFinalHeader marks the last attempt at a batch: the proxy will not send it again.
	DecisionFinalHeader = "Wardyn-Decision-Final"
	MaxDecisionBatch    = 128
)

// DecisionRecord is one entry of a batch post.
type DecisionRecord struct {
	ID          uuid.UUID          `json:"id"`
	Time        time.Time          `json:"time"` // when the proxy queued it
	Decision    egress.DecisionLog `json:"decision"`
	CPContact   string             `json:"cp_contact,omitempty"` // "lost", or empty
	ContactAgeS int64              `json:"contact_age_s,omitempty"`
}

// DecisionBatch is the body of a batch post.
type DecisionBatch struct {
	Records []DecisionRecord `json:"records"`
}

// maxBatchBody bounds a batch post's body, masked and with its wrapper: below an older control plane's 1 MiB
// cap on a decision post, so an older door always reads a batch as one empty decision and never refuses it.
const maxBatchBody = 960 << 10

// batchWrapper is the bytes a batch body adds around its records, commas aside.
const batchWrapper = len(`{"records":[`) + len(`]}`)

// decisionBatchBackoff is the wait before each retry of a batch post: four attempts in all.
var decisionBatchBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// queuedDecision is a decision waiting in the sink's queue: its id and queue time, which a retried batch keeps,
// and whether it was decided while the control plane could not be reached, and for how long it had not been.
type queuedDecision struct {
	log  egress.DecisionLog
	id   uuid.UUID
	at   time.Time
	lost bool
	ageS int64
}

// queued stamps log with an id, the queue time and the contact label.
func (s *decisionSink) queued(log egress.DecisionLog) queuedDecision {
	now := time.Now()
	q := queuedDecision{log: log, id: uuid.New(), at: now}
	if s.failing.Load() {
		q.lost = true
		q.ageS = max(0, int64(now.Sub(time.Unix(0, s.lastOK.Load()))/time.Second))
	}
	return q
}

// noteAnswer reads what an answer of the control plane says: the batch mode from DecisionBatchHeader (an
// integer in [2, 4096], capped at MaxDecisionBatch; anything else, or none, is single posts), and whether the
// control plane was reached.
func (s *decisionSink) noteAnswer(resp *http.Response) {
	n, err := strconv.ParseInt(resp.Header.Get(DecisionBatchHeader), 10, 64)
	if err != nil || n < 2 || n > 4096 {
		n = 0
	}
	s.batchMax.Store(min(n, MaxDecisionBatch))
	switch {
	case resp.StatusCode/100 == 2:
		s.lastOK.Store(time.Now().UnixNano())
		s.failing.Store(false)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode/100 == 5:
		s.failing.Store(true)
	}
}

// batchRecord is one encoded record of a batch, masked as it is posted.
type batchRecord struct {
	q    queuedDecision
	body []byte
}

func encodeBatchRecord(q queuedDecision) (batchRecord, error) {
	rec := DecisionRecord{ID: q.id, Time: q.at, Decision: q.log}
	if q.lost {
		rec.CPContact, rec.ContactAgeS = "lost", q.ageS
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return batchRecord{}, err
	}
	return batchRecord{q: q, body: maskDecisionBytes(b)}, nil
}

// postBatches posts first and the records queued behind it in batches while the control plane takes them. A
// usage record goes alone by post, in order, and so does a record that alone passes maxBatchBody; a batch ends
// where the relayed mark changes, since the mark is per post. Once the mode is single again, the rest go as the
// single loop sends them.
func (s *decisionSink) postBatches(first queuedDecision) {
	for next := &first; next != nil; {
		q := *next
		next = nil
		if s.batchMax.Load() < 2 {
			s.reportDropped(&s.reported)
			if err := s.post(q.log); err != nil {
				s.dropped.Add(1)
			}
			return
		}
		rec, err := encodeBatchRecord(q)
		if q.log.Usage != nil || err != nil || batchWrapper+len(rec.body) > maxBatchBody {
			if err := s.post(q.log); err != nil {
				s.dropped.Add(1)
			}
			return
		}
		next = s.postBatch(rec)
	}
}

// postBatch fills a batch led by first, posts it, and returns the queued record that starts the next batch, or
// nil. The summary of decisions dropped since the last one rides first in a batch that is not relayed (a single
// post never marks it relayed).
func (s *decisionSink) postBatch(first batchRecord) *queuedDecision {
	var recs []batchRecord
	size := batchWrapper
	relayed := first.q.log.Request.Relayed
	var summaryAt uint64
	if cur := s.dropped.Load(); cur > s.reported && !relayed {
		sq := queuedDecision{log: droppedSummaryLog(cur - s.reported), id: uuid.New(), at: time.Now()}
		if sr, err := encodeBatchRecord(sq); err == nil {
			recs, size, summaryAt = append(recs, sr), size+len(sr.body), cur
		}
	}
	add := func(r batchRecord) bool {
		extra := len(r.body)
		if len(recs) > 0 {
			extra++ // the comma
		}
		if size+extra > maxBatchBody || len(recs) >= int(s.batchMax.Load()) {
			return false
		}
		recs, size = append(recs, r), size+extra
		return true
	}
	var next *queuedDecision
	if !add(first) { // the summary and first do not fit together: first starts the next batch
		next = &first.q
	}
	for next == nil {
		var q queuedDecision
		ok := false
		select {
		case q, ok = <-s.ch:
		default:
		}
		if !ok {
			break // nothing more queued now, or the queue is closed: run reads the close itself
		}
		if q.log.Usage != nil || q.log.Request.Relayed != relayed {
			next = &q
			break
		}
		r, err := encodeBatchRecord(q)
		if err != nil || !add(r) {
			next = &q
		}
	}
	s.sendBatch(recs, relayed, summaryAt)
	return next
}

// sendBatch posts recs, at most four times with the same body and ids, the last attempt marked final. An
// answer without DecisionBatchHeader, whatever its status, means the control plane did not read a batch: the
// mode goes single and each record is posted alone. With the header, a 2xx landed; a 429 or 5xx, like no
// answer, is tried again and counted dropped after the last attempt; any other status is counted dropped at
// once, and a 400 (a malformed batch, never an older control plane) also logs one line. recs[0] is the drop
// summary when summaryAt is not 0; it is not a decision, so it is never counted dropped.
func (s *decisionSink) sendBatch(recs []batchRecord, relayed bool, summaryAt uint64) {
	var body bytes.Buffer
	body.WriteString(`{"records":[`)
	for i, r := range recs {
		if i > 0 {
			body.WriteByte(',')
		}
		body.Write(r.body)
	}
	body.WriteString(`]}`)
	decisions := uint64(len(recs))
	if summaryAt > 0 {
		decisions--
	}
	for attempt := 0; ; attempt++ {
		final := attempt == len(decisionBatchBackoff)
		code, advertised, err := s.postBatchBody(body.Bytes(), relayed, final)
		switch {
		case err == nil && !advertised:
			// The control plane did not read a batch: post each record as the single path does.
			s.batchMax.Store(0)
			for i, r := range recs {
				err := s.post(r.q.log)
				switch {
				case i == 0 && summaryAt > 0:
					if err == nil {
						s.reported = summaryAt
					}
				case err != nil:
					s.dropped.Add(1)
				}
			}
			return
		case err == nil && code/100 == 2:
			if summaryAt > 0 {
				s.reported = summaryAt
			}
			return
		case err == nil && code != http.StatusTooManyRequests && code/100 != 5:
			if code == http.StatusBadRequest {
				slog.Warn("egress proxy: the control plane refused a decision batch as malformed",
					slog.Int("status", code), slog.Uint64("records", decisions))
			}
			s.dropped.Add(decisions)
			return
		}
		if final {
			s.dropped.Add(decisions)
			return
		}
		time.Sleep(decisionBatchBackoff[attempt])
	}
}

// postBatchBody sends one attempt of a batch. It answers the status and whether the answer advertised
// batches, or the transport's error.
func (s *decisionSink) postBatchBody(body []byte, relayed, final bool) (int, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, false, err
	}
	req.Header.Set("Content-Type", DecisionBatchContentType)
	req.Header.Set("Authorization", "Bearer "+s.token.Get())
	if relayed {
		req.Header.Set(DecisionRelayedHeader, "1")
	}
	if final {
		req.Header.Set(DecisionFinalHeader, "1")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		s.failing.Store(true)
		return 0, false, fmt.Errorf("decision batch post: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	s.noteAnswer(resp)
	return resp.StatusCode, resp.Header.Get(DecisionBatchHeader) != "", nil
}
