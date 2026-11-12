// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package modelusage

// ollamaLine reads one NDJSON object. A non-stream answer is a single object, so one reader serves both.
// Only the object with done:true carries the counts; prompt_eval_count is omitted when the prompt was
// served from cache, which reads as zero; eval_count is required.
func (p *parser) ollamaLine(l []byte) {
	if len(l) == 0 {
		return
	}
	var o struct {
		Model           string `json:"model"`
		Done            bool   `json:"done"`
		PromptEvalCount int64  `json:"prompt_eval_count"`
		EvalCount       *int64 `json:"eval_count"`
	}
	if !p.decode(l, &o) {
		return
	}
	p.model(o.Model)
	if o.Done {
		p.figures(&o.PromptEvalCount, o.EvalCount, 0, 0, 0)
		p.terminal()
	}
}
