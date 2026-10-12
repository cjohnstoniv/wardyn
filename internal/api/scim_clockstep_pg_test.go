// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// A session issued in the same sub-second window as a later cut-off, with the wall clock stepping back
// between the two, is cut off like any other session issued before it.
func TestPG_SessionCutOffOrderingUnderABackwardClockStep(t *testing.T) {
	t.Skip("session cut-off ordering under a backward clock step: enabled when the ordering lands")
	e := newSCIMEnv(t)
	const sub, email = "sub-clockstep", "clockstep@corp.example"
	// The issue time reads 600ms ahead of now: the clock stepped back after the session was issued.
	c := scimCookieAt(t, sub, email, -1, time.Now().UTC().Add(600*time.Millisecond))
	if !e.cookieWorks(e.a, c) {
		t.Fatal("control: the session does not work before the cut-off")
	}
	if err := e.rev.RevokeSub(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond) // the stepped clock has passed the session's issue time
	for name, n := range map[string]*scimNode{"a": e.a, "b": e.b} {
		if r := doSSO(t, n.srv, http.MethodGet, "/api/v1/me", c, ""); r.Code == http.StatusOK {
			t.Errorf("instance %s: a session issued before the cut-off still works after it", name)
		}
	}
}
