// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package retrylog spaces out a recurring failure: its retries wait a doubling delay capped at Cap,
// and an identical message is logged once per such interval, with how often it repeated in between.
package retrylog

import "time"

// First is the wait after a first failure, and Cap the longest wait.
const (
	First = 5 * time.Second
	Cap   = 5 * time.Minute
)

// Delay is the wait after the n-th failure in a row (n >= 1): First, doubled for each further
// failure, at most Cap. 5 s, 10 s, 20 s, 40 s, 80 s, 160 s, then 5 min.
func Delay(n int) time.Duration {
	d := First
	for i := 1; i < n && d < Cap; i++ {
		d *= 2
	}
	return min(d, Cap)
}

// Dedupe decides which occurrences of a recurring message are logged. A message's first occurrence is
// logged; identical ones after it are counted, and the next is logged once Delay has passed, the
// delay growing with each line logged. A message not seen for twice Cap starts over. The zero value
// is ready; it is not safe for concurrent use.
type Dedupe struct{ seen map[string]*occurrence }

type occurrence struct {
	lines      int       // lines logged since it started
	next, last time.Time // when the next line is due; when it last occurred
	repeated   int       // occurrences not logged since the last line
}

// Note records msg occurring at now. It reports whether to log it, and how many identical
// occurrences since the previous line were not logged.
func (d *Dedupe) Note(msg string, now time.Time) (log bool, repeated int) {
	if d.seen == nil {
		d.seen = map[string]*occurrence{}
	}
	for k, o := range d.seen {
		if now.Sub(o.last) > 2*Cap {
			delete(d.seen, k)
		}
	}
	o := d.seen[msg]
	if o == nil {
		o = &occurrence{}
		d.seen[msg] = o
	}
	o.last = now
	if o.lines > 0 && now.Before(o.next) {
		o.repeated++
		return false, 0
	}
	o.lines++
	o.next = now.Add(Delay(o.lines))
	repeated, o.repeated = o.repeated, 0
	return true, repeated
}
