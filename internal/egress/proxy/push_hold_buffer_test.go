// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// heldPushCharge is how many bytes are charged to p's inspection budget now.
func heldPushCharge(p *Proxy) int64 {
	b := p.scan.init()
	free, most := int64(0), b.limit
	for free < most {
		mid := (free + most + 1) / 2
		if b.retained.TryAcquire(mid) {
			b.retained.Release(mid)
			free = mid
		} else {
			most = mid - 1
		}
	}
	return b.limit - free
}

// heldPushOf is a recorded push of a workflow file, which the review rule holds, beside size incompressible bytes.
func heldPushOf(t *testing.T, p *Proxy, size int) []byte {
	t.Helper()
	blob := make([]byte, size)
	_, _ = rand.NewChaCha8([32]byte{4, 0, 7}).Read(blob)
	return recordedPush(t, BranchNSPrefix(p.runID)+"work",
		map[string]string{".github/workflows/ci.yml": "on: push\n", "data.bin": string(blob)})
}

// TestHeldPushGivesBackItsBufferWhileHeld: a push held for review that is larger than a hold keeps in memory is
// not charged to the run's inspection budget while it waits, so the run's LLM inspection keeps its room. Once
// approved, the push is refused with a message git prints, saying to push the same commits again, and that push
// is forwarded whole without a second question.
func TestHeldPushGivesBackItsBufferWhileHeld(t *testing.T) {
	p, _, up, cp, _ := newAppLaneHold(t, reviewSpec(30, []string{".github/workflows/**"}), types.ApprovalPending)
	body := heldPushOf(t, p, 2<<20)
	got := make(chan *httptest.ResponseRecorder, 1)
	go func() { got <- postPush(t, p, string(body)) }()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if raises, polls := cp.counts(); raises == 1 && polls >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the push was never held")
		}
	}
	if n := heldPushCharge(p); n != 0 {
		t.Errorf("%d bytes stay charged to the run's inspection budget while a %d-byte push is held, want 0", n, len(body))
	}
	cp.set(types.ApprovalApproved)
	rec := <-got
	if up.gitHits != 0 || !strings.Contains(rec.Body.String(), "push the same commits again") {
		t.Fatalf("the approved push: status %d, %d forwards, body %q; want it refused, saying to push again", rec.Code, up.gitHits, rec.Body)
	}
	raises, _ := cp.counts()
	if rec := postPush(t, p, string(body)); rec.Code != http.StatusOK || !bytes.Equal(up.gitBody, body) {
		t.Fatalf("pushed again: status %d, the forge got %d of %d bytes; want it forwarded whole", rec.Code, len(up.gitBody), len(body))
	}
	if again, _ := cp.counts(); again != raises {
		t.Errorf("pushing the approved commits again asked again (%d raises, want %d)", again, raises)
	}
	if n := heldPushCharge(p); n != 0 {
		t.Errorf("%d bytes still charged after the push was forwarded", n)
	}
}
