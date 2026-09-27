// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package sidecar holds the small brokered-PUT plumbing shared by Wardyn's
// in-sandbox result-uploader binaries: validate WARDYN_PROXY_URL/
// WARDYN_RUN_ID, build the brokered result URL, and PUT a JSON body. No
// Authorization header is ever set here — the wardyn-proxy holds and
// injects the run token. The PUT goes DIRECT (Proxy: nil): it must never be
// sent through the proxy named by the sandbox's HTTP_PROXY.
package sidecar

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ProxyRunURL reads and validates WARDYN_PROXY_URL and WARDYN_RUN_ID from the
// environment and returns the brokered result URL for the given result kind
// (e.g. "scan", "verify"): "${WARDYN_PROXY_URL}/wardyn/v1/${kind}-results/${WARDYN_RUN_ID}".
func ProxyRunURL(kind string) (string, error) {
	proxyURL := strings.TrimRight(os.Getenv("WARDYN_PROXY_URL"), "/")
	if proxyURL == "" {
		return "", fmt.Errorf("WARDYN_PROXY_URL is required")
	}
	runID := os.Getenv("WARDYN_RUN_ID")
	if runID == "" {
		return "", fmt.Errorf("WARDYN_RUN_ID is required")
	}
	return proxyURL + "/wardyn/v1/" + kind + "-results/" + runID, nil
}

// Upload PUTs body (JSON) to url. No Authorization header is set on purpose;
// any non-2xx response is an error carrying a bounded snippet of the body.
func Upload(url string, body []byte) error {
	client := uploadClient()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http PUT: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// uploadClient builds the HTTP client every brokered PUT uses.
//
// Proxy: nil, not http.DefaultTransport: every sandbox carries
// HTTP_PROXY=$WARDYN_PROXY_URL, so DefaultTransport would send this
// control-plane PUT back through the proxy it names instead of reaching the
// run's own wardyn-proxy directly — invisible on the default proxy URL, but
// under a custom `--proxy-url` the PUT would go down the egress lane to a
// different host instead of the local brokered route.
func uploadClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{Proxy: nil},
		Timeout:   30 * time.Second,
	}
}
