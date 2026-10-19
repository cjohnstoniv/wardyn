// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// While an adopted sink's recovery stream is catching up, a runner OPEN from an offset the sink has
// not reached is turned away with a capacity reset (the runner retries); one at or below the sink's
// offset, or once recovery is over, is accepted and dedupes through accept's skip.
func TestHandleOpenDefersAnOpenAheadOfTheRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	link := &Link{}
	s := New("r1", link, Options{})
	org, run := runnerwire.Loopback()
	op := link.Attach(ctx, org)
	rp := runnerwire.NewPeer(run, runnerwire.PeerConfig{})
	go rp.Run(ctx)
	t.Cleanup(func() { rp.Close(); op.Close() })
	for _, tc := range []struct {
		name               string
		offset, open       int64
		recovering, capped bool
	}{
		{"ahead while recovering", 0, 1, true, true},
		{"ahead once recovered", 0, 1, false, false},
		{"at the sink while recovering", 5, 5, true, false},
		{"behind the sink while recovering", 5, 3, true, false},
	} {
		id := uuid.New()
		s.mu.Lock()
		s.outputs[id] = &outputSink{writer: &bytes.Buffer{}, offset: tc.offset, recovering: tc.recovering}
		s.mu.Unlock()
		st, err := rp.Open(ctx, runnerwire.Open{Kind: runnerwire.KindOutput, Target: id.String(), Offset: tc.open})
		var re *runnerwire.ResetError
		capped := errors.As(err, &re) && re.Code == runnerwire.ResetCapacity
		if capped != tc.capped || (!tc.capped && err != nil) {
			t.Errorf("%s: open = %v, want capacity reset %v", tc.name, err, tc.capped)
		}
		if st != nil {
			_ = st.Close()
		}
	}
}
