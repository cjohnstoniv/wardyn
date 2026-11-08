// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

// refusalRun is a run whose cast is stored, so a 404 can only come from the authorizer's answer.
const refusalRun = "0a000000-0000-4000-8000-000000000007"

const refusalCast = `{"version":2}` + "\n" + `[1.0,"o","secret\r\n"]` + "\n"

func refusalGet(t *testing.T, authorize recording.Authorizer) (int, string) {
	t.Helper()
	store, _ := recording.NewFSStore(t.TempDir())
	if err := store.SaveCast(context.Background(), refusalRun, strings.NewReader(refusalCast)); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newTestRouterWithAuth(store, authorize))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/runs/" + refusalRun + "/recording/" + refusalRun)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// An authorizer that writes its own refusal and returns false is answered with that refusal, never with the
// 404 an absent recording gets.
func TestHandlerAuthorizerWritesRefusal(t *testing.T) {
	code, body := refusalGet(t, func(w http.ResponseWriter, _ *http.Request, _ string) bool {
		http.Error(w, "kept in", http.StatusForbidden)
		return false
	})
	if code != http.StatusForbidden || !strings.Contains(body, "kept in") {
		t.Fatalf("got %d %q, want 403 with the authorizer's body", code, body)
	}
	if strings.Contains(body, "recording not found") {
		t.Errorf("the handler's 404 body was written over the refusal: %q", body)
	}
}

// An authorizer that returns false and writes nothing is the same 404 as before.
func TestHandlerAuthorizerSilentFalseIs404(t *testing.T) {
	code, body := refusalGet(t, func(http.ResponseWriter, *http.Request, string) bool { return false })
	if code != http.StatusNotFound || !strings.Contains(body, "recording not found") {
		t.Fatalf("got %d %q, want 404 recording not found", code, body)
	}
}

// An authorizer that returns true and writes nothing is served.
func TestHandlerAuthorizerSilentTrueServes(t *testing.T) {
	code, body := refusalGet(t, func(http.ResponseWriter, *http.Request, string) bool { return true })
	if code != http.StatusOK || body != refusalCast {
		t.Fatalf("got %d %q, want 200 with the cast", code, body)
	}
}

// An authorizer that writes a refusal and then returns true serves no cast: the client gets the refusal only.
func TestHandlerAuthorizerWroteAndAllowedServesNoCast(t *testing.T) {
	code, body := refusalGet(t, func(w http.ResponseWriter, _ *http.Request, _ string) bool {
		http.Error(w, "kept in", http.StatusForbidden)
		return true
	})
	if code != http.StatusForbidden {
		t.Fatalf("got %d, want the authorizer's 403", code)
	}
	if strings.Contains(body, "secret") || strings.Contains(body, `"version"`) {
		t.Errorf("cast bytes reached the client after the authorizer wrote: %q", body)
	}
	if !strings.Contains(body, "kept in") {
		t.Errorf("body %q lost the authorizer's refusal", body)
	}
}
