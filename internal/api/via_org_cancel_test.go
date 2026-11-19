// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A stream the idle tick or the run's end cancels while its model upload has stopped arriving returns at
// once, not when the body's 5-minute read bound runs out: the engine's handler returns, the stream is
// released, and the retired engine then closes.
func TestViaOrg_IdleAndRunEndEndAStalledUpload(t *testing.T) {
	const host = "api.anthropic.com"
	start := `{"model":`
	for _, end := range []string{"idle", "run end"} {
		t.Run(end, func(t *testing.T) {
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			s := &Server{cfg: Config{BaseCtx: ctx}, bgWaitBudget: 10 * time.Second}
			eng, err := proxy.NewViaOrgEngine(context.Background(), &proxy.Config{
				RunID: uuid.New(), ControlPlaneURL: "http://127.0.0.1:9", RunToken: "run-token", DecisionBufferSize: 16,
				Policy:        types.RunPolicySpec{AllowedDomains: []string{host}},
				AllowedModels: &[]string{"model-a"},
			}, proxy.ViaOrgEngineOptions{})
			if err != nil {
				t.Fatal(err)
			}
			gen := &viaOrgGen{engine: eng}
			run := &viaOrgRun{id: uuid.New(), runner: uuid.New(), bytes: new(atomic.Int64), done: make(chan struct{}),
				streams: map[*viaOrgStream]struct{}{}, idleSince: time.Now(), engine: gen,
				byteCap: viaOrgMaxBytes, idle: time.Minute, period: time.Hour}
			st, refused, retired := run.open(ctx, gen)
			if st == nil || refused != "" || retired {
				t.Fatalf("open = %v %q retired=%v, want a stream", st, refused, retired)
			}

			// The engine is served as handleViaOrg serves it: the stream's context, the counted body and writer.
			served := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(served)
				defer run.release(st)
				out := viaOrgEngineRequest(r.WithContext(st.ctx), "/v1/messages", "", viaOrgDestination{host: host}, st, false)
				gen.engine.ServeInjected(&viaOrgCountingWriter{ResponseWriter: w, st: st}, out, host)
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
			if _, err := stalled.Write([]byte(start)); err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(10 * time.Second); run.bytes.Load() < int64(len(start)); {
				if time.Now().After(deadline) {
					t.Fatal("the engine never read the upload's first bytes")
				}
				time.Sleep(10 * time.Millisecond)
			}

			if end == "idle" {
				run.closeIdle(time.Now().Add(2 * run.idle)) // ends the stream: no byte for longer than the timeout
			} else {
				s.releaseViaOrgRun(run, true, true)
			}
			select {
			case <-served:
			case <-time.After(5 * time.Second):
				t.Fatalf("after the %s the handler was still reading an upload that stopped arriving", end)
			}
			run.smu.Lock()
			open := len(run.streams)
			run.smu.Unlock()
			if open != 0 {
				t.Fatalf("the handler returned with %d streams still open", open)
			}
			if end == "idle" {
				retiredGen := run.closeIdle(time.Now().Add(2 * run.idle))
				if retiredGen != gen {
					t.Fatalf("the idle tick retired %v, want the run's engine generation", retiredGen)
				}
				closeViaOrgGen(run.id, retiredGen)
			}
			for deadline := time.Now().Add(15 * time.Second); !gen.engine.Closed(); {
				if time.Now().After(deadline) {
					t.Fatalf("the engine did not close after the %s", end)
				}
				time.Sleep(10 * time.Millisecond)
			}
			s.bg.Wait()
		})
	}
}
