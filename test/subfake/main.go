// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command subfake stands in for TWO real endpoints in #677's hermetic
// Docker+Postgres per-person-subscription test
// (internal/api/provider_subscription_docker_test.go), for the same reason
// test/conformance/hopfake exists: reachability between a real wardyn-proxy
// container and this repo's own go-test process is unreliable on this box's
// Docker Desktop/WSL2 setup (host.docker.internal flakes; --network host does
// not share the WSL loopback there — see that file's own measurements). So,
// like hopfake, this runs INSIDE a container on the SAME docker network as the
// proxy under test, never reachable to or from a real vendor.
//
//   - the control plane's credential resolve (GET
//     /api/v1/internal/injection/{SUBFAKE_GRANT}): answers with the ONE
//     {header, value} pair given by SUBFAKE_HEADER/SUBFAKE_VALUE — the REAL,
//     Postgres-sourced per-owner token the test's own Go process already
//     resolved via the real dispatch code before ever touching Docker — or,
//     with SUBFAKE_REFUSE=1, a 403 (simulating the real control plane's
//     owner-mismatch refusal).
//   - the vendor leg (any other path): records the Authorization header the
//     proxy actually forwarded, so the test can assert exactly whose token
//     reached "the vendor" and that a refused resolve never reaches it at all.
//
// A TEST BINARY: the test builds it and copies it into a throwaway
// container; nothing packages or publishes it.
package main

import (
	"crypto/tls"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
)

func main() {
	cert, err := tls.X509KeyPair([]byte(os.Getenv("SUBFAKE_CERT_PEM")), []byte(os.Getenv("SUBFAKE_KEY_PEM")))
	if err != nil {
		log.Fatalf("subfake: key pair: %v", err)
	}
	grantPath := "/api/v1/internal/injection/" + os.Getenv("SUBFAKE_GRANT")
	header, value := os.Getenv("SUBFAKE_HEADER"), os.Getenv("SUBFAKE_VALUE")
	refuse := os.Getenv("SUBFAKE_REFUSE") == "1"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == grantPath {
			if refuse {
				log.Printf("subfake: resolve grant=%s REFUSED (simulated owner mismatch)", strings.TrimPrefix(r.URL.Path, "/api/v1/internal/injection/"))
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"owner mismatch"}`))
				return
			}
			log.Printf("subfake: resolve grant=%s -> header=%q", strings.TrimPrefix(r.URL.Path, "/api/v1/internal/injection/"), header)
			_ = json.NewEncoder(w).Encode(map[string]string{"header": header, "value": value})
			return
		}
		// The vendor leg: log exactly what the proxy forwarded, unformatted.
		log.Printf("subfake: vendor request %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_fake","type":"message","role":"assistant","content":[]}`))
	})
	srv := &http.Server{
		Addr:      ":8443",
		Handler:   handler,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ErrorLog:  log.New(os.Stdout, "subfake: ", 0),
	}
	log.Fatal(srv.ListenAndServeTLS("", ""))
}
