// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import "sync"

// startFence lets exec starts through until the stop scrub closes it. A start holds the read lock from
// its check through the process start, so close, which takes the write lock, returns only after every
// start it let through has started its process or failed, and refuses every start after it. Starts do
// not wait for each other; only close waits.
type startFence struct {
	mu     sync.RWMutex
	refuse error // set by close
}

// enter lets a start through, or refuses it with close's error. A start let through calls leave once
// its process has started or failed to.
func (f *startFence) enter() error {
	f.mu.RLock()
	if err := f.refuse; err != nil {
		f.mu.RUnlock()
		return err
	}
	return nil
}

func (f *startFence) leave() { f.mu.RUnlock() }

// close waits for every start already let through to leave, then refuses later ones with refuse.
func (f *startFence) close(refuse error) {
	f.mu.Lock()
	f.refuse = refuse
	f.mu.Unlock()
}
