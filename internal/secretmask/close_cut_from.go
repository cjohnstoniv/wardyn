// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretmask

// CloseCutFrom is CloseCut for a trailing run of floor bytes or more; a shorter one is flushed as Close does.
func (w *MaskingWriter) CloseCutFrom(floor int) error {
	cut := w.m.cutPrefixLen(w.tail)
	if cut < floor {
		cut = 0
	}
	return w.flush(cut)
}
