// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A forwarded header-injected request reaches the engine through the control plane's listener, which
// sets no read timeout, as a broker request does. The run's allowed_models reads a model request's body
// before any later gate re-arms a deadline, so an upload that stops arriving must end there under the
// bound a terminated tunnel's body has, instead of holding the request, and the engine's close, for good.
func TestViaOrg_InjectedBodyReadIsBounded(t *testing.T) {
	prev := mitmReadTimeout
	mitmReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { mitmReadTimeout = prev })
	f := newViaOrgFixture(t, nil, func(c *Config) {
		c.Policy.AllowedDomains = append(c.Policy.AllowedDomains, anthropicHost)
	})
	f.engine.srv.proxy.allowedModels = compileAllowedModels(&[]string{"model-a"}, nil)
	served := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(served)
		r.Host = anthropicHost
		f.engine.ServeInjected(w, r, anthropicHost)
	}))
	defer srv.Close()
	body, stalled := io.Pipe()
	defer stalled.Close()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", body)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		if resp, err := srv.Client().Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	if _, err := stalled.Write([]byte(`{"model":`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("an injected model request whose body stopped arriving was still being read")
	}
	if hits, _, _ := f.up.seen(); hits != 0 {
		t.Fatalf("a model request whose body never finished reached the upstream %d times", hits)
	}
}
