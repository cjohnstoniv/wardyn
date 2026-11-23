// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// A person holds at most assistantStreamsPerPerson streams; a release frees one, and another person is not held
// by the first one's count.
func TestAssistantStreamCap(t *testing.T) {
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

// Over the cap the events route answers 429 before it streams.
func TestAssistantStreamOverTheCapIsRefused(t *testing.T) {
	site, bind := assistantTwoProviders()
	f := newAssistantFake(site)
	srv, _ := assistantSessionServer(t, f)
	person := "sub-cap-" + uuid.NewString()
	cookie := ssoSession(t, person, person+"@corp.example", oidc.RoleUser)
	s := assistantLiveSession(person, nil, bind)
	f.seed(s)
	for i := 0; i < assistantStreamsPerPerson; i++ {
		release, ok := assistantStreamTake(person)
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
