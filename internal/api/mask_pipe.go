// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "sync"

// maskPipeMax bounds the bytes a maskPipe holds that its masker has not taken
// yet. A batch costs one read of the shared registry (at most one per 50ms per
// replica), so this over that interval is the most one pipe masks per second:
// 512 KiB a batch is about 8 MB/s, past what a browser terminal renders.
const maskPipeMax = 512 << 10

// maskPipe is the front of a live masker whose producer must not wait on it: a
// live session's recording (the attach and SSH shell pumps tee their output into
// it) and a batch run's output tail (the drivers' copy of the agent's output).
// liveMaskWriter forwards nothing until a read of the shared masking registry
// that began after the bytes arrived has finished, and those reads are coalesced
// to one per 50ms per replica, so a producer that wrote each chunk straight into
// it read its source once per read: about 20 chunks a second.
//
// Write queues and returns. One goroutine hands everything queued to the masker
// in a single Write, in arrival order, and the next batch gathers while that
// Write's read runs. That read began after every byte of its batch arrived, so a
// batch is vouched for exactly as a single chunk was; the masker's holdback joins
// a secret split across two batches as it joins one split across two chunks; and
// a read that fails replaces the batch with the placeholder. A Write that would
// take the queue past maskPipeMax waits, so a pump the masker cannot keep up with
// stops reading the exec, and a paused pump queues nothing.
type maskPipe struct {
	mw *liveMaskWriter

	mu      sync.Mutex
	moved   *sync.Cond // the queue was taken, or a batch was written
	queue   []byte
	queued  int64 // bytes ever queued
	written int64 // bytes ever handed to mw
	running bool  // a drain goroutine owns the queue
}

func newMaskPipe(mw *liveMaskWriter) *maskPipe {
	p := &maskPipe{mw: mw}
	p.moved = sync.NewCond(&p.mu)
	return p
}

// Write never fails: the recording is best-effort and must not break the relay.
func (p *maskPipe) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.queue) > 0 && len(p.queue)+len(b) > maskPipeMax {
		p.moved.Wait()
	}
	p.queue = append(p.queue, b...)
	p.queued += int64(len(b))
	if !p.running {
		p.running = true
		go p.drain()
	}
	return len(b), nil
}

// drain writes batches until the queue is empty, then returns: an idle pipe holds
// no goroutine and no buffer. Two buffers alternate, one being masked while the
// other fills; io.Writer's contract is that mw keeps neither.
func (p *maskPipe) drain() {
	var spare []byte
	p.mu.Lock()
	for len(p.queue) > 0 {
		batch := p.queue
		p.queue = spare[:0]
		p.moved.Broadcast()
		p.mu.Unlock()
		_, _ = p.mw.Write(batch)
		p.mu.Lock()
		p.written += int64(len(batch))
		spare = batch
		p.moved.Broadcast()
	}
	p.queue, p.running = nil, false
	p.mu.Unlock()
}

// flush waits until every byte queued before it has been through the masker.
func (p *maskPipe) flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for target := p.queued; p.written < target; {
		p.moved.Wait()
	}
}
