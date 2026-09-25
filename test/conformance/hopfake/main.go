// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command hopfake stands in for wardynd's proxy-facing TLS listener in two
// docker conformance cases. In TestProxyHopTLSDocker it serves TLS with the
// leaf the test minted from its own internal CA (internal/hoptls), answers one
// injection grant for one run token, and logs every request with whether it
// arrived over TLS. In TestRecordingDocker it is the internal recording-upload
// endpoint: it logs each cast the proxy brokers (size, whether it carries the
// test's canary, and whether it arrived over TLS). A TEST BINARY: the test
// builds it and copies it into a throwaway busybox container; nothing packages
// or publishes it.
package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

const recordingsPath = "/api/v1/internal/recordings/"

func main() {
	cert, err := tls.X509KeyPair([]byte(os.Getenv("HOPFAKE_CERT_PEM")), []byte(os.Getenv("HOPFAKE_KEY_PEM")))
	if err != nil {
		log.Fatalf("hopfake: key pair: %v", err)
	}
	grantPath := "/api/v1/internal/injection/" + os.Getenv("HOPFAKE_GRANT")
	wantAuth := "Bearer " + os.Getenv("HOPFAKE_TOKEN")
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, ok := strings.CutPrefix(r.URL.Path, recordingsPath); ok {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			fmt.Printf("hopfake: recording run=%s bytes=%d canary=%t tls=%t\n",
				id, len(body), bytes.Contains(body, []byte("wardyn-rec-canary")), r.TLS != nil)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != grantPath {
			fmt.Printf("hopfake: %s %s tls=%t\n", r.Method, r.URL.Path, r.TLS != nil)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if r.Header.Get("Authorization") != wantAuth {
			fmt.Printf("hopfake: resolve refused: bad run token tls=%t\n", r.TLS != nil)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Printf("hopfake: resolve grant=%s tls=%t\n", strings.TrimPrefix(r.URL.Path, "/api/v1/internal/injection/"), r.TLS != nil)
		_ = json.NewEncoder(w).Encode(map[string]string{"header": "X-Api-Key", "value": "hop-canary-credential"})
	})
	srv := &http.Server{
		Addr:      ":8443",
		Handler:   handler,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13},
		ErrorLog:  log.New(os.Stdout, "hopfake: ", 0), // TLS handshake failures land in docker logs
	}
	log.Fatal(srv.ListenAndServeTLS("", ""))
}
