// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command fakeorg is the K6 kind harness's minimal organisation endpoint
// (scripts/k6-kind-conformance.sh). It answers a runner's registration with
// the runner ID the harness chose, so the chart's register init container and
// runnerd's boot checks run unchanged under the rendered Role. It holds no hub:
// every stream request gets 503, because org-runner hub sessions are lane K3's.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func main() {
	orgURL := os.Getenv("K6_ORG_URL")
	runnerID, err := uuid.Parse(os.Getenv("K6_RUNNER_ID"))
	if err != nil || orgURL == "" {
		log.Fatalf("fakeorg: K6_ORG_URL and K6_RUNNER_ID are required (%v)", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/runners/register", func(w http.ResponseWriter, r *http.Request) {
		var req types.RunnerRegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.PublicKey) == 0 {
			http.Error(w, "bad registration", http.StatusBadRequest)
			return
		}
		log.Printf("fakeorg: registered runner %s name=%q arch=%q", runnerID, req.Name, req.Arch)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(types.RunnerRegistration{
			RunnerID: runnerID, Fingerprint: runnerwire.Fingerprint(req.PublicKey),
			State: types.RunnerUnclaimed, OrgURLSHA256: federation.OrgURLSHA256(orgURL), Limits: req.Limits,
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stream") {
			log.Printf("fakeorg: stream attempt %s", r.URL.Path)
			http.Error(w, "k6 fake org: no hub (org-runner sessions are lane K3's)", http.StatusServiceUnavailable)
			return
		}
		log.Printf("fakeorg: unhandled %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	log.Fatal(http.ListenAndServeTLS(":8443", "/tls/tls.crt", "/tls/tls.key", mux))
}
