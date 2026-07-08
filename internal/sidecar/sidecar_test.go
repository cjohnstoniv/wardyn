// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sidecar

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProxyRunURL(t *testing.T) {
	t.Setenv("WARDYN_PROXY_URL", "")
	t.Setenv("WARDYN_RUN_ID", "")
	if _, err := ProxyRunURL("scan"); err == nil {
		t.Fatal("expected error when WARDYN_PROXY_URL is unset")
	}
	t.Setenv("WARDYN_PROXY_URL", "http://proxy:3128/")
	if _, err := ProxyRunURL("scan"); err == nil {
		t.Fatal("expected error when WARDYN_RUN_ID is unset")
	}
	t.Setenv("WARDYN_RUN_ID", "run-1")
	url, err := ProxyRunURL("scan")
	if err != nil {
		t.Fatalf("ProxyRunURL: %v", err)
	}
	if want := "http://proxy:3128/wardyn/v1/scan-results/run-1"; url != want {
		t.Errorf("ProxyRunURL = %q, want %q", url, want)
	}
}

func TestUpload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"ok":true}` {
			t.Errorf("body = %s", body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := Upload(srv.URL, []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv2.Close()
	if err := Upload(srv2.URL, []byte(`{}`)); err == nil {
		t.Fatal("expected error on non-2xx response")
	}
}
