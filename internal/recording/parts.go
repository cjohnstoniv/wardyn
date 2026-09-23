// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
)

// A long run's cast arrives in parts (RL-12): wardyn-rec tails its own cast and
// uploads it every 24 h or 64 MiB, each part the cast's header line followed by
// whole event lines. Part 1 is stored under the bare run id — exactly where a
// short run's single upload has always landed — and part n >= 2 under
// CastKey(runID, PartSuffix(n)). OpenJoined and StatJoined put them back
// together; nothing else needs to know a cast was split.

// PartSuffix is the cast-key suffix part n (n >= 2) of a run's cast is stored
// under. It cannot collide with an attach session's suffix, which is a UUID.
func PartSuffix(n int) string { return "part-" + strconv.Itoa(n) }

// OpenJoined opens runID's cast as one asciicast v2 document: part 1 as stored,
// then each later part with its repeated header line dropped, until a part is
// missing. A part whose header differs from part 1's belongs to an earlier cast
// of the same run (a revive starts a new cast and replaces part 1), so the join
// ends there rather than splicing two recordings. Parts are opened one at a
// time, so a store that buffers a whole cast (PGStore) holds one part, not the
// run.
//
// ponytail: part 1 anchors the join, so retention sweeping part 1 of a run that
// outlived the retention window hides its later parts from replay; a part index
// would lift that if retention and year-long runs ever meet.
func OpenJoined(ctx context.Context, s Store, runID string) (io.ReadCloser, error) {
	rc, err := s.OpenCast(ctx, runID)
	if err != nil {
		return nil, err
	}
	br := bufio.NewReader(rc)
	header, err := br.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		_ = rc.Close()
		return nil, err
	}
	return &joinedCast{
		ctx: ctx, s: s, runID: runID, header: header, next: 2,
		cur: rc, r: io.MultiReader(bytes.NewReader(header), br),
	}, nil
}

type joinedCast struct {
	ctx    context.Context
	s      Store
	runID  string
	header []byte
	next   int
	cur    io.Closer
	r      io.Reader
}

func (j *joinedCast) Read(p []byte) (int, error) {
	for {
		n, err := j.r.Read(p)
		if !errors.Is(err, io.EOF) {
			return n, err
		}
		more, oerr := j.openNext()
		if oerr != nil {
			return n, oerr
		}
		if !more {
			return n, io.EOF
		}
		if n > 0 {
			return n, nil
		}
	}
}

// openNext advances to the next part, reporting false when the join has ended.
func (j *joinedCast) openNext() (bool, error) {
	if j.cur == nil {
		return false, nil
	}
	_ = j.cur.Close()
	j.cur = nil
	// A cast without a complete header line was never split.
	if !bytes.HasSuffix(j.header, []byte("\n")) {
		return false, nil
	}
	rc, err := j.s.OpenCast(j.ctx, CastKey(j.runID, PartSuffix(j.next)))
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	br := bufio.NewReader(rc)
	if h, err := br.ReadBytes('\n'); err != nil || !bytes.Equal(h, j.header) {
		_ = rc.Close()
		return false, nil
	}
	j.next++
	j.cur, j.r = rc, br
	return true, nil
}

func (j *joinedCast) Close() error {
	if j.cur == nil {
		return nil
	}
	err := j.cur.Close()
	j.cur = nil
	return err
}

// StatJoined is StatAndTail over every part of runID's cast: the size is the
// bytes stored across all of them, and the tail is the last part's — event
// times run on from part to part because every part is cut from one cast file.
//
// ponytail: unlike OpenJoined it does not read each part's header, so after a
// revive it still counts an earlier cast's later parts until they are swept;
// replay itself stays correct.
func StatJoined(ctx context.Context, s Store, runID string, tailBytes int64) (int64, []byte, error) {
	size, tail, err := s.StatAndTail(ctx, runID, tailBytes)
	if err != nil {
		return 0, nil, err
	}
	for n := 2; ; n++ {
		ps, pt, err := s.StatAndTail(ctx, CastKey(runID, PartSuffix(n)), tailBytes)
		if errors.Is(err, ErrNotFound) {
			return size, tail, nil
		}
		if err != nil {
			return 0, nil, err
		}
		size += ps
		tail = pt
	}
}
