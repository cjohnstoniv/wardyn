// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "net/http"

// MetricsListenerHandler is the whole handler of the dedicated metrics
// listener (-metrics-listen): GET /metrics, with no credential, and a 404 for
// every other method and path. It exists for a stock Prometheus that scrapes
// without a bearer token.
//
// It is a separate handler on a separate http.Server, never a route on the
// console's router: /metrics there keeps its operator gate (routes.go), and
// nothing else the console serves (no /healthz, no base path, no API) is
// reachable here. handleMetrics reads nothing from the auth context, so the
// body is the same one the gated route answers. Who can reach the port is the
// deployment's call (the chart's NetworkPolicy rule, threatmodel/THREAT-MODEL.md).
func (s *Server) MetricsListenerHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		s.handleMetrics(w, r)
	})
}
