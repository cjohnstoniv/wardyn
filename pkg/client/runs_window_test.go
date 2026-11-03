// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOpenMacWindowPostsCLITrigger(t *testing.T) {
	id := uuid.New()
	var path, method, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		path, method, body = r.URL.Path, r.Method, string(b)
		_, _ = w.Write([]byte(`{"grant_id":"` + uuid.NewString() + `","runner_id":"` + uuid.NewString() + `","runner":"studio","trigger":"cli"}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "t", HTTPClient: srv.Client()}
	g, err := c.OpenMacWindow(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/api/v1/runs/"+id.String()+"/desktop/window" || body != `{"trigger":"cli"}` || g.Runner != "studio" {
		t.Fatalf("got %s %s %q %+v", method, path, body, g)
	}
}

func TestOpenMacWindowRefusalIsAnAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"Only a macOS VM run has a window."}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "t", HTTPClient: srv.Client()}
	_, err := c.OpenMacWindow(context.Background(), uuid.New())
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusForbidden {
		t.Fatalf("err = %v, want a 403 *APIError", err)
	}
}

func TestGetMacWindowReadsTheRunsView(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		at := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
		_, _ = w.Write([]byte(`{"mac_window":{"state":"open","last_close":{"reason":"closed_by_person","at":"` + at + `"}}}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "t", HTTPClient: srv.Client()}
	v, err := c.GetMacWindow(context.Background(), uuid.New())
	if err != nil || v == nil || v.State != "open" || v.LastClose == nil || v.LastClose.Reason != "closed_by_person" {
		t.Fatalf("view = %+v, err = %v", v, err)
	}
}
