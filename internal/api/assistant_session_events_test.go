// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A person holds at most assistantStreamsPerPerson streams and long-polls; a release frees one, and another person
// is not held by the first one's count.
func TestAssistantStreamCap(t *testing.T) {
	var l assistantLive
	assistantStreamTake := l.take
	person, other := "sub-streams-"+uuid.NewString(), "sub-streams-"+uuid.NewString()
	var releases []func()
	for i := 0; i < assistantStreamsPerPerson; i++ {
		release, ok := assistantStreamTake(person)
		if !ok {
			t.Fatalf("stream %d refused under the cap", i+1)
		}
		releases = append(releases, release)
	}
	if _, ok := assistantStreamTake(person); ok {
		t.Fatal("a stream over the cap was taken")
	}
	release, ok := assistantStreamTake(other)
	if !ok {
		t.Fatal("another person was held by the first one's streams")
	}
	release()
	releases[0]()
	again, ok := assistantStreamTake(person)
	if !ok {
		t.Fatal("a released slot was not freed")
	}
	again()
	for _, r := range releases[1:] {
		r()
	}
}

// Over the cap the events route answers 429 before it streams, and a plain long-poll is counted as a stream is.
func TestAssistantStreamOverTheCapIsRefused(t *testing.T) {
	site, bind := assistantTwoProviders()
	f := newAssistantFake(site)
	srv, _ := assistantSessionServer(t, f)
	person := "sub-cap-" + uuid.NewString()
	cookie := ssoSession(t, person, person+"@corp.example", oidc.RoleUser)
	s := assistantLiveSession(person, nil, bind)
	f.seed(s)
	for i := 0; i < assistantStreamsPerPerson; i++ {
		release, ok := srv.assistantLive.take(person)
		if !ok {
			t.Fatal("could not hold the cap")
		}
		defer release()
	}
	r := httptest.NewRequest(http.MethodGet, assistantPath(s.ID, "/events"), nil)
	r.Header.Set("Accept", "text/event-stream")
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	wantReason(t, w, http.StatusTooManyRequests, reasonAssistantRateLimited)
	wantReason(t, doSSO(t, srv, http.MethodGet, assistantPath(s.ID, "/events"), cookie, ""), http.StatusTooManyRequests, reasonAssistantRateLimited)
}

// A wake ends every wait on its id and no other; a release after the wake, or with none, leaves nothing behind.
func TestAssistantLiveWake(t *testing.T) {
	var l assistantLive
	a, b := uuid.New(), uuid.New()
	wa, ra := l.watch(a)
	wa2, ra2 := l.watch(a)
	wb, rb := l.watch(b)
	l.wake(a, uuid.Nil)
	for i, ch := range []<-chan struct{}{wa, wa2} {
		select {
		case <-ch:
		default:
			t.Errorf("wait %d on the woken id did not end", i)
		}
	}
	select {
	case <-wb:
		t.Error("a wait on another id ended")
	default:
	}
	ra()
	ra2()
	rb()
	if n := len(l.waits); n != 0 {
		t.Errorf("%d waits left after every release", n)
	}
}

// An open long-poll answers when the person's message is stored, long before its deadline: it waits for the
// session's wake, not for a timer that reads the store.
func TestAssistantLongPollAnswersOnTheWake(t *testing.T) {
	site, bind := assistantTwoProviders()
	f := newAssistantFake(site)
	srv, _ := assistantSessionServer(t, f)
	person := "sub-wake-" + uuid.NewString()
	cookie := ssoSession(t, person, person+"@corp.example", oidc.RoleUser)
	s := assistantLiveSession(person, nil, bind)
	f.seed(s)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doSSO(t, srv, http.MethodGet, assistantPath(s.ID, "/events"), cookie, "") }()
	time.Sleep(time.Second) // the poll has read the store and is waiting
	start := time.Now()
	if w := doSSO(t, srv, http.MethodPost, assistantPath(s.ID, "/messages"), cookie, `{"text":"hello"}`); w.Code != http.StatusAccepted {
		t.Fatalf("message = %d %s", w.Code, w.Body)
	}
	select {
	case w := <-done:
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), types.AssistantEventUserMessage) {
			t.Fatalf("the open poll answered %d %s, want the message", w.Code, w.Body)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("the poll answered %s after the message", d)
		}
	case <-time.After(assistantLongPoll / 2):
		t.Fatal("the open poll did not answer on the wake")
	}
}

// A reconnect's Last-Event-ID moves the start past the cursor, never before it; a header that is not a number is
// ignored.
func TestAssistantStreamAfterReadsLastEventID(t *testing.T) {
	cases := []struct {
		header string
		after  int64
		want   int64
	}{
		{"", 3, 3},
		{"9", 3, 9},
		{"2", 3, 3},
		{"x", 3, 3},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if c.header != "" {
			r.Header.Set("Last-Event-ID", c.header)
		}
		if got := assistantStreamAfter(r, c.after); got != c.want {
			t.Errorf("Last-Event-ID %q after %d = %d, want %d", c.header, c.after, got, c.want)
		}
	}
}
