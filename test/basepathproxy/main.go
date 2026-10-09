// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command basepathproxy is the reverse proxy the Playwright e2e backend puts in
// front of wardynd in its base-path mode (scripts/e2e-backend.sh,
// WARDYN_E2E_BASE_PATH). It does what an operator's proxy does for
// WARDYN_BASE_PATH: every request under -prefix goes to -target with its path
// and Host unchanged (WebSocket upgrades included), and the rest of the host
// answers as a neighbouring application would. A TEST BINARY: the harness
// builds it into .e2e-bin; nothing packages or publishes it.
package main

import (
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// neighbourBody is what the host answers outside the prefix, so a spec can
// tell "wardynd 404" from "never reached wardynd".
const neighbourBody = "basepathproxy: outside the base path (a neighbouring application)"

func main() {
	listen := flag.String("listen", "127.0.0.1:8090", "address to listen on")
	target := flag.String("target", "http://127.0.0.1:8088", "wardynd's console listener")
	prefix := flag.String("prefix", "/wardyn", "the WARDYN_BASE_PATH wardynd serves under")
	flag.Parse()
	upstream, err := url.Parse(*target)
	if err != nil || upstream.Host == "" {
		log.Fatalf("basepathproxy: -target %q is not an absolute URL", *target)
	}
	rp := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(upstream)
		pr.Out.Host = pr.In.Host
	}}
	srv := &http.Server{
		Addr:              *listen,
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == *prefix || strings.HasPrefix(r.URL.Path, *prefix+"/") {
				rp.ServeHTTP(w, r)
				return
			}
			http.Error(w, neighbourBody, http.StatusNotFound)
		}),
	}
	log.Printf("basepathproxy: %s%s -> %s", *listen, *prefix, *target)
	log.Fatal(srv.ListenAndServe())
}
