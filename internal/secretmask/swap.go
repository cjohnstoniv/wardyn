// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretmask

import "github.com/google/uuid"

// JSONVariantMaskerGen is JSONVariantMasker with the generation it was built at, both read under one lock: the
// Masker is the corpus at exactly that generation. A caller that keeps the Masker compares the generation later
// to learn whether the corpus moved since.
func (r *Registry) JSONVariantMaskerGen(runID uuid.UUID) (Masker, uint64) {
	if r == nil {
		return Masker{}, 0
	}
	r.mu.RLock()
	if c := r.cached[runID]; c != nil && c.gen == r.gen && c.haveVariant {
		m, gen := c.variant, c.gen
		r.mu.RUnlock()
		return m, gen
	}
	r.mu.RUnlock()
	c := r.rebuild(runID, true)
	return c.variant, c.gen
}

// SetMasker swaps the Masker the writer masks with. It forwards nothing: the held bytes stay as they arrived, and
// so does the count of them a placeholder already forwarded stands for, and the next Write or Close judges both
// with m. A secret m adds that begins in the held bytes is masked whole; one that begins in bytes already forwarded
// is not, so the caller swaps before writing any byte that arrived after m's corpus last changed.
func (w *MaskingWriter) SetMasker(m Masker) { w.m = m }
