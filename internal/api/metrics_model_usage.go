// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"io"
	"math"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// modelTokenVendors is the closed set of vendor labels on wardyn_model_tokens_total. A value outside it
// counts under "other", so the series count stays fixed whatever a proxy sends. The metric has no run label.
var modelTokenVendors = []string{"anthropic", "openai", "bedrock", "ollama", "other"}

// modelTokenDirections are the label values for the five counts a record carries.
var modelTokenDirections = []string{"input", "output", "cache_read", "cache_write", "reasoning"}

// modelTokens adds one stored usage record's counts. A record with no counts adds nothing. A count that would
// take its series past an int64 is not added: the series stays the exact sum of the counts it took and is never
// wrapped negative.
func (m *metrics) modelTokens(u egress.ModelUsage) {
	vendor := u.Vendor
	if !slices.Contains(modelTokenVendors, vendor) {
		vendor = "other"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.modelTok == nil {
		m.modelTok = map[[2]string]int64{}
	}
	for i, n := range []int64{u.Input, u.Output, u.CacheRead, u.CacheWrite, u.Reasoning} {
		key := [2]string{vendor, modelTokenDirections[i]}
		if n > 0 && n <= math.MaxInt64-m.modelTok[key] {
			m.modelTok[key] += n
		}
	}
}

// writeModelTokens emits wardyn_model_tokens_total; the caller holds m.mu. Omitted until a count arrives.
func (m *metrics) writeModelTokens(w io.Writer) {
	if len(m.modelTok) == 0 {
		return
	}
	fmt.Fprint(w, "# HELP wardyn_model_tokens_total Model tokens counted from providers' own answers, by vendor and direction. Input is as the vendor reports it: Anthropic and Bedrock exclude cache tokens, OpenAI includes cached tokens.\n"+
		"# TYPE wardyn_model_tokens_total counter\n")
	for _, vendor := range modelTokenVendors {
		for _, dir := range modelTokenDirections {
			if n, ok := m.modelTok[[2]string{vendor, dir}]; ok {
				fmt.Fprintf(w, "wardyn_model_tokens_total{vendor=%q,direction=%q} %d\n", vendor, dir, n)
			}
		}
	}
}
