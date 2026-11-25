// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// cpPost is one post a stub control plane received.
type cpPost struct {
	batch   bool
	final   bool
	relayed bool
	auth    string
	body    []byte
	single  egress.DecisionLog
	records []DecisionRecord
}

// stubCP is a decision door. answer picks each post's status and whether the answer advertises batches; nil
// answers 202 with the advertisement.
type stubCP struct {
	mu     sync.Mutex
	posts  []cpPost
	answer func(i int, p cpPost) (int, bool)
	delay  time.Duration
	srv    *httptest.Server
}

func newStubCP(t *testing.T, answer func(i int, p cpPost) (int, bool)) *stubCP {
	t.Helper()
	c := &stubCP{answer: answer}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p := cpPost{
			batch:   r.Header.Get("Content-Type") == DecisionBatchContentType,
			final:   r.Header.Get(DecisionFinalHeader) == "1",
			relayed: r.Header.Get(DecisionRelayedHeader) == "1",
			auth:    r.Header.Get("Authorization"),
			body:    body,
		}
		if p.batch {
			var b DecisionBatch
			_ = json.Unmarshal(body, &b)
			p.records = b.Records
		} else {
			_ = json.Unmarshal(body, &p.single)
		}
		if c.delay > 0 {
			time.Sleep(c.delay)
		}
		c.mu.Lock()
		i := len(c.posts)
		c.posts = append(c.posts, p)
		c.mu.Unlock()
		status, advertise := http.StatusAccepted, true
		if c.answer != nil {
			status, advertise = c.answer(i, p)
		}
		if advertise {
			w.Header().Set(DecisionBatchHeader, strconv.Itoa(MaxDecisionBatch))
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *stubCP) got() []cpPost {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]cpPost(nil), c.posts...)
}

// pausedSink is a sink whose worker has not started, so the records queued before start are read in one go.
func pausedSink(c *stubCP, n int, token string) (*decisionSink, func()) {
	s := &decisionSink{
		endpoint: controlPlaneEndpoint(c.srv.URL, "/api/v1/internal/decisions"),
		token:    newTokenSource(token),
		client:   c.srv.Client(),
		ch:       make(chan queuedDecision, n),
	}
	s.lastOK.Store(time.Now().UnixNano())
	return s, func() { s.wg.Add(1); go s.run() }
}

// fastBackoff shortens the retry waits for the test.
func fastBackoff(t *testing.T) {
	t.Helper()
	prev := decisionBatchBackoff
	decisionBatchBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { decisionBatchBackoff = prev })
}

func hostLog(i int) egress.DecisionLog {
	return decisionLog(egress.Request{RunID: uuid.New(), Host: fmt.Sprintf("h%d.test", i), Method: "GET"}, egress.Allow, "policy:allowed")
}

func closeSink(t *testing.T, s *decisionSink) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// close answers an error naming the drop count when there were drops; each test checks the count itself.
	if err := s.close(ctx); err != nil && !strings.Contains(err.Error(), "dropped records") {
		t.Fatalf("close: %v", err)
	}
}

// shape prints the posts as "s" (single) or "bN" (a batch of N), in order.
func shape(posts []cpPost) string {
	var parts []string
	for _, p := range posts {
		if p.batch {
			parts = append(parts, fmt.Sprintf("b%d", len(p.records)))
		} else {
			parts = append(parts, "s")
		}
	}
	return strings.Join(parts, " ")
}

func waitPosts(t *testing.T, c *stubCP, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(c.got()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("stub saw %d posts, want %d", len(c.got()), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDecisionSinkBatch_NeverBatchesUntilAdvertised(t *testing.T) {
	quiet := newStubCP(t, func(int, cpPost) (int, bool) { return http.StatusAccepted, false })
	s, start := pausedSink(quiet, 16, "tok")
	for i := range 6 {
		s.emit(hostLog(i))
	}
	start()
	closeSink(t, s)
	if got := shape(quiet.got()); got != "s s s s s s" {
		t.Fatalf("no advertisement: posts %q, want six single posts", got)
	}

	loud := newStubCP(t, nil)
	s, start = pausedSink(loud, 16, "tok")
	for i := range 6 {
		s.emit(hostLog(i))
	}
	start()
	closeSink(t, s)
	if got := shape(loud.got()); got != "s b5" {
		t.Fatalf("advertised on the first answer: posts %q, want %q", got, "s b5")
	}
}

func TestDecisionSinkBatch_OlderControlPlaneRepostsSingly(t *testing.T) {
	var mu sync.Mutex
	var singles []string
	empty := 0
	// The first answer comes from a control plane that took batches; every later one from an older door: it reads
	// one decision with unknown fields allowed, answers 202, and never advertises.
	c := newStubCP(t, func(i int, p cpPost) (int, bool) {
		var d egress.DecisionLog
		if err := types.DecodeOneJSON(bytes.NewReader(p.body), &d, false); err != nil {
			return http.StatusBadRequest, false
		}
		mu.Lock()
		defer mu.Unlock()
		if d.Request.Host == "" {
			empty++
		} else {
			singles = append(singles, d.Request.Host)
		}
		return http.StatusAccepted, i == 0
	})
	s, start := pausedSink(c, 16, "tok")
	for i := range 6 {
		s.emit(hostLog(i))
	}
	start()
	waitPosts(t, c, 7)
	for i := 6; i < 9; i++ {
		s.emit(hostLog(i))
	}
	closeSink(t, s)
	if got := shape(c.got()); got != "s b5 s s s s s s s s" {
		t.Fatalf("posts %q, want one single, the batch, then every record singly", got)
	}
	if empty != 1 {
		t.Fatalf("the older door read %d empty decisions, want 1 (the batch)", empty)
	}
	want := "h0.test h1.test h2.test h3.test h4.test h5.test h6.test h7.test h8.test"
	if got := strings.Join(singles, " "); got != want {
		t.Fatalf("decisions the older door kept: %q, want %q", got, want)
	}
	if m := s.batchMax.Load(); m != 0 {
		t.Fatalf("mode after an older door's answer: %d, want 0 (single)", m)
	}
	if d := s.droppedCount(); d != 0 {
		t.Fatalf("dropped %d, want 0", d)
	}
}

func TestDecisionSinkBatch_DropsBackWhenAdvertisementGone(t *testing.T) {
	c := newStubCP(t, func(i int, _ cpPost) (int, bool) { return http.StatusAccepted, i < 2 })
	s, start := pausedSink(c, 16, "tok")
	s.emit(hostLog(1))
	s.emit(hostLog(2))
	usage := hostLog(3)
	usage.Usage = &egress.ModelUsage{CallID: uuid.New(), Vendor: "anthropic", Host: "h3.test"}
	s.emit(usage)
	s.emit(hostLog(4))
	s.emit(hostLog(5))
	start()
	closeSink(t, s)
	// The usage record goes alone; its answer carries no advertisement, so the rest go singly.
	if got := shape(c.got()); got != "s b1 s s s" {
		t.Fatalf("posts %q, want %q", got, "s b1 s s s")
	}
	if m := s.batchMax.Load(); m != 0 {
		t.Fatalf("mode %d, want 0", m)
	}
}

func TestDecisionSinkBatch_RetrySameIDsFinalMarkOnLast(t *testing.T) {
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}; fmt.Sprint(decisionBatchBackoff) != fmt.Sprint(want) {
		t.Fatalf("backoff %v, want %v", decisionBatchBackoff, want)
	}
	fastBackoff(t)
	for _, tc := range []struct {
		name     string
		fails    int // batch attempts answered 503 before one lands
		attempts int
		dropped  uint64
	}{
		{"all four fail", 4, 4, 2},
		{"third lands", 2, 3, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			batches := 0
			c := newStubCP(t, func(_ int, p cpPost) (int, bool) {
				if !p.batch {
					return http.StatusAccepted, true
				}
				mu.Lock()
				defer mu.Unlock()
				batches++
				if batches <= tc.fails {
					return http.StatusServiceUnavailable, true
				}
				return http.StatusAccepted, true
			})
			s, start := pausedSink(c, 16, "tok")
			for i := range 3 {
				s.emit(hostLog(i))
			}
			start()
			closeSink(t, s) // waits for the worker, so its last answer is counted before the checks below
			var tries []cpPost
			for _, p := range c.got() {
				if p.batch {
					tries = append(tries, p)
				}
			}
			if len(tries) != tc.attempts {
				t.Fatalf("batch attempts %d, want %d", len(tries), tc.attempts)
			}
			for i, p := range tries {
				if !bytes.Equal(p.body, tries[0].body) {
					t.Fatalf("attempt %d body differs from the first", i+1)
				}
				if len(p.records) != 2 || p.records[0].ID == uuid.Nil || p.records[0].ID == p.records[1].ID {
					t.Fatalf("attempt %d records %+v: want two with distinct ids", i+1, p.records)
				}
				if wantFinal := i == len(decisionBatchBackoff); p.final != wantFinal {
					t.Fatalf("attempt %d final mark %v, want %v", i+1, p.final, wantFinal)
				}
			}
			if d := s.droppedCount(); d != tc.dropped {
				t.Fatalf("dropped %d, want %d", d, tc.dropped)
			}
		})
	}
}

func TestDecisionSinkBatch_StatusTable(t *testing.T) {
	fastBackoff(t)
	for _, tc := range []struct {
		status    int
		advertise bool
		attempts  int
		singles   int // records posted again one by one
		mode      int64
		warns     int
	}{
		{http.StatusTooManyRequests, true, 4, 0, MaxDecisionBatch, 0},
		{http.StatusInternalServerError, true, 4, 0, MaxDecisionBatch, 0},
		{http.StatusBadGateway, true, 4, 0, MaxDecisionBatch, 0},
		{http.StatusServiceUnavailable, true, 4, 0, MaxDecisionBatch, 0},
		{http.StatusBadRequest, true, 1, 0, MaxDecisionBatch, 1},
		{http.StatusUnauthorized, true, 1, 0, MaxDecisionBatch, 0},
		{http.StatusForbidden, true, 1, 0, MaxDecisionBatch, 0},
		{http.StatusNotFound, true, 1, 0, MaxDecisionBatch, 0},
		{http.StatusRequestEntityTooLarge, true, 1, 0, MaxDecisionBatch, 0},
		{http.StatusServiceUnavailable, false, 1, 2, 0, 0},
		{http.StatusOK, false, 1, 2, 0, 0},
	} {
		t.Run(fmt.Sprintf("%d advertised=%v", tc.status, tc.advertise), func(t *testing.T) {
			logs := captureSlog(t)
			c := newStubCP(t, func(i int, p cpPost) (int, bool) {
				switch {
				case i == 0:
					return http.StatusAccepted, true
				case p.batch:
					return tc.status, tc.advertise
				default: // a single post after the batch
					return tc.status, tc.advertise
				}
			})
			s, start := pausedSink(c, 16, "tok")
			for i := range 3 {
				s.emit(hostLog(i))
			}
			start()
			closeSink(t, s) // waits for the worker: every post and count below is final
			posts := c.got()
			batches, singles := 0, 0
			for _, p := range posts[1:] {
				switch {
				case p.batch:
					batches++
				case strings.HasPrefix(p.single.RuleSource, "egress:dropped-decisions-"):
					// close's own summary of the drops; not a record of the batch
				default:
					singles++
				}
			}
			if batches != tc.attempts || singles != tc.singles {
				t.Fatalf("posts %q: %d batch attempts and %d single posts, want %d and %d", shape(posts), batches, singles, tc.attempts, tc.singles)
			}
			wantDropped := uint64(2)
			if tc.status/100 == 2 {
				wantDropped = 0
			}
			if d := s.droppedCount(); d != wantDropped {
				t.Fatalf("dropped %d, want %d", d, wantDropped)
			}
			if m := s.batchMax.Load(); m != tc.mode {
				t.Fatalf("mode %d, want %d", m, tc.mode)
			}
			if n := strings.Count(logs.String(), "refused a decision batch"); n != tc.warns {
				t.Fatalf("warn lines %d, want %d: %s", n, tc.warns, logs)
			}
		})
	}
}

func TestDecisionSinkBatch_ContactLostLabel(t *testing.T) {
	c := newStubCP(t, func(i int, _ cpPost) (int, bool) {
		if i == 0 {
			return http.StatusServiceUnavailable, true
		}
		return http.StatusAccepted, true
	})
	s := newDecisionSink(c.srv.URL, newTokenSource("tok"), 16, c.srv.Client(), nil)
	s.emit(hostLog(1))
	waitPosts(t, c, 1)
	for !s.failing.Load() {
		time.Sleep(time.Millisecond)
	}
	s.lastOK.Store(time.Now().Add(-90 * time.Second).UnixNano())
	s.emit(hostLog(2)) // decided while the control plane could not be reached
	waitPosts(t, c, 2)
	for s.failing.Load() {
		time.Sleep(time.Millisecond)
	}
	s.emit(hostLog(3))
	closeSink(t, s)
	posts := c.got()
	// The first decision's 503 counted it dropped, so the summary leads the next batch.
	if got := shape(posts); got != "s b2 b1" {
		t.Fatalf("posts %q, want %q", got, "s b2 b1")
	}
	if !strings.HasPrefix(posts[1].records[0].Decision.RuleSource, "egress:dropped-decisions-1") {
		t.Fatalf("the second post does not lead with the drop summary: %+v", posts[1].records[0])
	}
	lost, back := posts[1].records[1], posts[2].records[0]
	if lost.Decision.Request.Host != "h2.test" || back.Decision.Request.Host != "h3.test" {
		t.Fatalf("records %q and %q, want h2.test and h3.test", lost.Decision.Request.Host, back.Decision.Request.Host)
	}
	if lost.CPContact != "lost" || lost.ContactAgeS < 90 {
		t.Fatalf("decision made during the outage: cp_contact %q age %d, want lost and >= 90", lost.CPContact, lost.ContactAgeS)
	}
	if back.CPContact != "" || back.ContactAgeS != 0 {
		t.Fatalf("decision made after contact returned: cp_contact %q age %d, want none", back.CPContact, back.ContactAgeS)
	}

	// No answer at all also counts as lost contact.
	d := &decisionSink{endpoint: "http://127.0.0.1:1/api/v1/internal/decisions", token: newTokenSource("tok"), client: &http.Client{Timeout: time.Second}}
	if err := d.post(hostLog(4)); err == nil || !d.failing.Load() {
		t.Fatalf("post where nothing listens: err %v failing %v, want an error and failing", err, d.failing.Load())
	}
}

func TestDecisionSinkBatch_RelayedSplitsBatches(t *testing.T) {
	c := newStubCP(t, nil)
	s, start := pausedSink(c, 16, "tok")
	for i, relayed := range []bool{false, false, false, true, true, false} {
		l := hostLog(i)
		l.Request.Relayed = relayed
		s.emit(l)
	}
	start()
	closeSink(t, s)
	posts := c.got()
	if got := shape(posts); got != "s b2 b2 b1" {
		t.Fatalf("posts %q, want %q", got, "s b2 b2 b1")
	}
	for i, want := range []bool{false, false, true, false} {
		if posts[i].relayed != want {
			t.Fatalf("post %d relayed mark %v, want %v", i, posts[i].relayed, want)
		}
	}
}

func TestDecisionSink_EmitStampsQueueTime(t *testing.T) {
	c := newStubCP(t, nil)
	s, start := pausedSink(c, 16, "tok")
	decided := time.Now().Add(-time.Minute)
	before := time.Now()
	for i := range 3 {
		l := hostLog(i)
		l.Request.Time = decided
		s.emit(l)
	}
	after := time.Now()
	start()
	closeSink(t, s)
	posts := c.got()
	if got := shape(posts); got != "s b2" {
		t.Fatalf("posts %q, want %q", got, "s b2")
	}
	seen := map[uuid.UUID]bool{}
	for _, r := range posts[1].records {
		if r.Time.Before(before) || r.Time.After(after) {
			t.Fatalf("record time %v outside the queue window [%v, %v]", r.Time, before, after)
		}
		if !r.Decision.Request.Time.Equal(decided) {
			t.Fatalf("decision time %v, want %v unchanged", r.Decision.Request.Time, decided)
		}
		if r.ID == uuid.Nil || seen[r.ID] {
			t.Fatalf("record id %v: want a fresh id per record", r.ID)
		}
		seen[r.ID] = true
	}
}

func TestDecisionSinkBatch_BodyNeverPassesCap(t *testing.T) {
	c := newStubCP(t, nil)
	s, start := pausedSink(c, 16, "tok")
	sized := func(i, n int) egress.DecisionLog {
		l := hostLog(i)
		l.RuleSource = strings.Repeat("a", n)
		return l
	}
	s.emit(hostLog(0))
	for i := 1; i <= 5; i++ {
		s.emit(sized(i, 300<<10))
	}
	s.emit(sized(6, 1<<20)) // alone passes the cap
	s.emit(hostLog(7))
	s.emit(hostLog(8))
	start()
	closeSink(t, s)
	hosts := map[string]bool{}
	for _, p := range c.got() {
		if p.batch {
			if len(p.body) > maxBatchBody {
				t.Fatalf("batch body %d bytes passes the %d cap", len(p.body), maxBatchBody)
			}
			for _, r := range p.records {
				hosts[r.Decision.Request.Host] = true
			}
			continue
		}
		hosts[p.single.Request.Host] = true
		if p.single.Request.Host == "h6.test" && len(p.body) <= maxBatchBody {
			t.Fatalf("the oversized record posted singly in %d bytes, want over the cap", len(p.body))
		}
	}
	for i := range 9 {
		if !hosts[fmt.Sprintf("h%d.test", i)] {
			t.Fatalf("record h%d.test never arrived; posts %q", i, shape(c.got()))
		}
	}
	if got := shape(c.got()); got != "s b3 b2 s b2" {
		t.Fatalf("posts %q, want %q", got, "s b3 b2 s b2")
	}
}

func TestDecisionSinkBatch_HeaderlessAfterCommitDoublesNeverLoses(t *testing.T) {
	c := newStubCP(t, func(i int, p cpPost) (int, bool) {
		switch {
		case i == 0:
			return http.StatusAccepted, true
		case p.batch: // stored, then an intermediary answers
			return http.StatusBadGateway, false
		default:
			return http.StatusAccepted, false
		}
	})
	s, start := pausedSink(c, 16, "tok")
	for i := range 5 {
		s.emit(hostLog(i))
	}
	start()
	closeSink(t, s)
	inBatch, single := map[string]int{}, map[string]int{}
	for _, p := range c.got()[1:] {
		for _, r := range p.records {
			inBatch[r.Decision.Request.Host]++
		}
		if !p.batch {
			single[p.single.Request.Host]++
		}
	}
	for i := 1; i < 5; i++ {
		h := fmt.Sprintf("h%d.test", i)
		if inBatch[h] != 1 || single[h] != 1 {
			t.Fatalf("%s: %d in the batch and %d single, want once each", h, inBatch[h], single[h])
		}
	}
	if d := s.droppedCount(); d != 0 {
		t.Fatalf("dropped %d, want 0", d)
	}
}

func TestDecisionSinkBatch_DropSummaryReportedOnce(t *testing.T) {
	var mu sync.Mutex
	summaries := 0
	c := newStubCP(t, func(_ int, p cpPost) (int, bool) {
		var d egress.DecisionLog
		_ = types.DecodeOneJSON(bytes.NewReader(p.body), &d, false)
		if strings.HasPrefix(d.RuleSource, "egress:dropped-decisions-") {
			mu.Lock()
			summaries++
			mu.Unlock()
		}
		return http.StatusAccepted, false
	})
	s, start := pausedSink(c, 16, "tok")
	s.batchMax.Store(MaxDecisionBatch) // an earlier answer advertised batches
	s.dropped.Store(7)
	s.emit(hostLog(1))
	s.emit(hostLog(2))
	start()
	closeSink(t, s)
	posts := c.got()
	if got := shape(posts); got != "b3 s s s" {
		t.Fatalf("posts %q, want the batch led by the summary, then each record singly", got)
	}
	if !strings.Contains(string(posts[0].body), "egress:dropped-decisions-7") {
		t.Fatalf("the batch does not lead with the summary: %.200s", posts[0].body)
	}
	if summaries != 1 {
		t.Fatalf("the door saw the summary %d times, want 1", summaries)
	}
	if s.reported != 7 {
		t.Fatalf("reported %d, want 7", s.reported)
	}
}

func TestDecisionSinkBatch_LogsNoSecrets(t *testing.T) {
	fastBackoff(t)
	const token, host, path = "tok-SECRET-4063", "secret-host.example", "/secret/path"
	logs := captureSlog(t)
	for _, answer := range []func(int, cpPost) (int, bool){
		func(i int, p cpPost) (int, bool) {
			return map[bool]int{true: http.StatusBadRequest, false: http.StatusAccepted}[p.batch], true
		},
		func(i int, p cpPost) (int, bool) {
			return map[bool]int{true: http.StatusServiceUnavailable, false: http.StatusAccepted}[p.batch], true
		},
		func(i int, _ cpPost) (int, bool) { return http.StatusBadGateway, i == 0 },
	} {
		c := newStubCP(t, answer)
		s, start := pausedSink(c, 16, token)
		for range 3 {
			s.emit(decisionLog(egress.Request{RunID: uuid.New(), Host: host, Path: path, Method: "GET"}, egress.Deny, "policy:denied"))
		}
		start()
		closeSink(t, s)
	}
	out := logs.String()
	if !strings.Contains(out, "refused a decision batch") {
		t.Fatalf("the capture saw no line; the test proves nothing: %q", out)
	}
	for _, secret := range []string{token, host, path} {
		if strings.Contains(out, secret) {
			t.Fatalf("a log line names %q: %s", secret, out)
		}
	}
}

func TestDecisionSink_DropsUnderLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("10 s of load")
	}
	run := func(advertise bool) uint64 {
		c := newStubCP(t, func(int, cpPost) (int, bool) { return http.StatusAccepted, advertise })
		c.delay = 5 * time.Millisecond
		s := newDecisionSink(c.srv.URL, newTokenSource("tok"), defaultBufferSize, c.srv.Client(), nil)
		began := time.Now()
		for i := range 5000 {
			s.emit(hostLog(i))
			if i%50 == 49 {
				time.Sleep(time.Until(began.Add(time.Duration(i+1) * time.Millisecond)))
			}
		}
		dropped := s.droppedCount()
		closeSink(t, s)
		return dropped
	}
	batched, single := run(true), run(false)
	t.Logf("5000 decisions in 5 s, 5 ms per answer: dropped %d with batching, %d with single posts", batched, single)
	if batched > single {
		t.Fatalf("batching dropped %d, more than single posts' %d", batched, single)
	}
}

// TestDecisionSinkBatch_SummaryNeverHoldsBackARecord: a record that fits the cap alone but not beside the pending
// drop summary goes first, by itself, and the summary rides later; a 4xx on a batch that carried the summary is
// followed by a wait, never by an immediate post.
func TestDecisionSinkBatch_SummaryNeverHoldsBackARecord(t *testing.T) {
	c := newStubCP(t, nil)
	s, start := pausedSink(c, 16, "tok")
	s.batchMax.Store(MaxDecisionBatch)
	s.dropped.Store(3)
	big := hostLog(1)
	base, err := encodeBatchRecord(queuedDecision{log: big, id: uuid.New(), at: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	big.RuleSource = strings.Repeat("a", maxBatchBody-batchWrapper-len(base.body)-10) // fits alone by 10 bytes
	s.emit(big)
	start()
	closeSink(t, s)
	posts := c.got()
	if len(posts) == 0 || !posts[0].batch || len(posts[0].records) != 1 || posts[0].records[0].Decision.Request.Host != "h1.test" {
		t.Fatalf("posts %q: want the record alone in the first batch", shape(posts))
	}
	if len(posts[0].body) > maxBatchBody {
		t.Fatalf("the first batch is %d bytes, over the cap", len(posts[0].body))
	}
	summaries := 0
	for _, p := range posts {
		for _, r := range p.records {
			if strings.HasPrefix(r.Decision.RuleSource, "egress:dropped-decisions-3") {
				summaries++
			}
		}
		if strings.HasPrefix(p.single.RuleSource, "egress:dropped-decisions-3") {
			summaries++
		}
	}
	if summaries != 1 || s.reported != 3 {
		t.Fatalf("summary posted %d times, reported %d: want it once, later, and reported 3", summaries, s.reported)
	}

	prev := decisionBatchBackoff
	decisionBatchBackoff = []time.Duration{100 * time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { decisionBatchBackoff = prev })
	var mu sync.Mutex
	var at []time.Time
	refusing := newStubCP(t, func(_ int, p cpPost) (int, bool) {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		if p.batch {
			return http.StatusForbidden, true
		}
		return http.StatusAccepted, true
	})
	s, start = pausedSink(refusing, 16, "tok")
	s.batchMax.Store(MaxDecisionBatch)
	s.dropped.Store(2)
	s.emit(hostLog(2))
	start()
	closeSink(t, s)
	if got := shape(refusing.got()); got != "b2 s" {
		t.Fatalf("posts %q, want the refused batch with the summary, then close's summary", got)
	}
	if gap := at[1].Sub(at[0]); gap < 100*time.Millisecond {
		t.Fatalf("the next post came %v after a refused summary batch, want a wait of the first backoff", gap)
	}
}
