// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"io"
)

// OTLPPushConfig is the Config part for the OTLP metrics push's own series.
type OTLPPushConfig struct {
	// OTLPMetricsPushFailures, when set, reports how many OTLP metrics pushes did not reach the
	// collector, for wardyn_otlp_metrics_push_failures_total (cmd/wardynd wires it to the
	// otlp_metrics push). Nil omits the metric: the push is off.
	OTLPMetricsPushFailures func() int64
}

// writeOTLPPushFailures emits wardyn_otlp_metrics_push_failures_total: OTLP metrics pushes that
// did not reach the collector. Omitted when the push is off (OTLPMetricsPushFailures nil). A failed
// push is not retried, so this counter is what shows a dead push after the next good one.
func (s *Server) writeOTLPPushFailures(w io.Writer) {
	if s.cfg.OTLPMetricsPushFailures == nil {
		return
	}
	fmt.Fprintf(w, "# HELP wardyn_otlp_metrics_push_failures_total OTLP metrics pushes that did not reach the collector (encode error, transport error or non-2xx reply).\n"+
		"# TYPE wardyn_otlp_metrics_push_failures_total counter\nwardyn_otlp_metrics_push_failures_total %d\n", s.cfg.OTLPMetricsPushFailures())
}
