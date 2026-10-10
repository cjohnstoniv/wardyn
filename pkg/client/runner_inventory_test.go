// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestRunnerInventoryMethodsHitTheirRoutes(t *testing.T) {
	id := uuid.New()
	var got []string
	var putBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.RequestURI())
		if r.Method == http.MethodPut {
			b, _ := io.ReadAll(r.Body)
			putBody = string(b)
		}
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/api/v1/runners/settings":
			_, _ = w.Write([]byte(`{"enabled":true}`))
		case r.URL.Path == "/api/v1/runners/"+id.String():
			_, _ = w.Write([]byte(`{"id":"` + id.String() + `","state":"claimed","online":true,"runs_active":2}`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(srv.Close)
	c := client.New(srv.URL, "tok")
	ctx := context.Background()

	if _, err := c.ListRunners(ctx, client.RunnerFilterRevoked); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListRunners(ctx, ""); err != nil {
		t.Fatal(err)
	}
	v, err := c.GetRunner(ctx, id)
	if err != nil || v.ID != id || !v.Online || v.RunsActive != 2 {
		t.Fatalf("GetRunner = %+v, %v", v, err)
	}
	if _, err := c.ListMyRunners(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListRunnerTokens(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.RevokeRunnerToken(ctx, id); err != nil {
		t.Fatal(err)
	}
	if s, err := c.GetRunnerSettings(ctx); err != nil || !s.Enabled {
		t.Fatalf("GetRunnerSettings = %+v, %v", s, err)
	}
	if _, err := c.SetRunnersEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /api/v1/runners?state=revoked", "GET /api/v1/runners", "GET /api/v1/runners/" + id.String(),
		"GET /api/v1/me/runners", "GET /api/v1/runners/tokens", "DELETE /api/v1/runners/tokens/" + id.String(),
		"GET /api/v1/runners/settings", "PUT /api/v1/runners/settings",
	}
	if len(got) != len(want) {
		t.Fatalf("requests %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d = %q, want %q", i, got[i], want[i])
		}
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(putBody), &body); err != nil || body["enabled"] != false {
		t.Fatalf("turning off must send an explicit enabled:false, sent %q", putBody)
	}
}
