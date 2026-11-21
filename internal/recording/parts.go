// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A long run's cast arrives in parts (RL-12): wardyn-rec tails its own cast and
// uploads it every 24 h or 32 MiB, each part the cast's header line followed by
// whole event lines. Part 1 is stored under the bare run id — exactly where a
// short run's single upload has always landed — and part n >= 2 under
// CastKey(runID, PartSuffix(n)). OpenJoined and StatJoined put them back
// together; nothing else needs to know a cast was split.

// FormatHeader is the upload header wardyn-rec sets, to FormatLog, when the body is a plain log rather than
// a cast. Absent (an older wardyn-rec, or a cast) the server tells them apart by the body's first bytes.
const (
	FormatHeader = "X-Wardyn-Recording-Format"
	FormatLog    = "log"
)

// LogCutMarker is the line wardyn-rec appends to a plain log it uploads cut short: the first keep of size
// bytes, then this.
func LogCutMarker(keep, size int64) string {
	return fmt.Sprintf("\n[wardyn-rec: log truncated: the first %d of %d bytes were uploaded; the rest was not]\n", keep, size)
}

// LogCutMarkerMax is the longest LogCutMarker.
var LogCutMarkerMax = len(LogCutMarker(math.MaxInt64, math.MaxInt64))

var logCutMarker = regexp.MustCompile(`\n\[wardyn-rec: log truncated: the first [0-9]+ of [0-9]+ bytes were uploaded; the rest was not\]\n\z`)

// LogCutAt is where a LogCutMarker that ends b starts, or -1.
func LogCutAt(b []byte) int {
	if loc := logCutMarker.FindIndex(b); loc != nil {
		return loc[0]
	}
	return -1
}

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
	header, err := joinedHeader(br)
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
	// No part above the upload bound was ever stored, so the join never reads past it.
	if j.next > types.RecordingMaxParts {
		return false, nil
	}
	rc, err := j.s.OpenCast(continuingReplay(j.ctx), CastKey(j.runID, PartSuffix(j.next)))
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	br := bufio.NewReader(rc)
	// A part that cannot be read is an error, as part 1's is (OpenJoined): ending the join there would serve
	// a shorter cast as if it were the whole one. A part cut short or of an earlier cast ends it.
	h, err := joinedHeader(br)
	if err != nil && !errors.Is(err, io.EOF) {
		_ = rc.Close()
		return false, err
	}
	if !bytes.Equal(h, j.header) {
		_ = rc.Close()
		return false, nil
	}
	j.next++
	j.cur, j.r = rc, br
	return true, nil
}

func joinedHeader(br *bufio.Reader) ([]byte, error) {
	var header []byte
	for {
		part, err := br.ReadSlice('\n')
		// FS recordings can bypass HTTP's part cap; do not allocate an
		// unbounded header before the output decoder gets to enforce its cap.
		if len(header)+len(part) > maxCastBytes {
			return nil, ErrInvalidCast
		}
		header = append(header, part...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return header, err
		}
	}
}

func (j *joinedCast) Close() error {
	if j.cur == nil {
		return nil
	}
	err := j.cur.Close()
	j.cur = nil
	return err
}

// StatJoined is StatAndTail over the parts of runID's cast that OpenJoined plays: the size is the bytes stored
// across them, and the tail is the last one's — event times run on from part to part because every part is cut
// from one cast file. Like the replay, it ends at the first part whose header is not part 1's: that part belongs
// to an earlier cast of the run, so after a revive its bytes and duration are not the recording's. Each part's
// header is read with a bounded head read (castHeadProbe bytes), not the part.
//
// ponytail: a header longer than castHeadProbe is compared by its first castHeadProbe bytes.
func StatJoined(ctx context.Context, s Store, runID string, tailBytes int64) (int64, []byte, error) {
	size, tail, err := s.StatAndTail(ctx, runID, tailBytes)
	if err != nil {
		return 0, nil, err
	}
	head, err := headOf(ctx, s, runID)
	if err != nil {
		return 0, nil, err
	}
	header, whole := castHeaderOf(head)
	if !whole {
		return size, tail, nil // a cast without a complete header line was never split
	}
	for n := 2; n <= types.RecordingMaxParts; n++ {
		key := CastKey(runID, PartSuffix(n))
		ps, pt, err := s.StatAndTail(ctx, key, tailBytes)
		if errors.Is(err, ErrNotFound) {
			return size, tail, nil
		}
		if err != nil {
			return 0, nil, err
		}
		ph, err := headOf(ctx, s, key)
		if errors.Is(err, ErrNotFound) {
			return size, tail, nil
		}
		if err != nil {
			return 0, nil, err
		}
		if h, _ := castHeaderOf(ph); !bytes.Equal(h, header) {
			return size, tail, nil
		}
		size += ps
		tail = pt
	}
	return size, tail, nil
}

// castHeadProbe bounds the head read that tells which cast a part belongs to; a cast header is one short JSON line.
const castHeadProbe = 16 << 10

// castHeaderOf is the header line at the start of head, newline included, and whether it is complete: a head
// that fills the probe without a newline counts as complete, so such a header is compared by its probe.
func castHeaderOf(head []byte) ([]byte, bool) {
	if i := bytes.IndexByte(head, '\n'); i >= 0 {
		return head[:i+1], true
	}
	return head, len(head) >= castHeadProbe
}

// headCaster is a Store that can read the first bytes of a cast without opening it whole.
type headCaster interface {
	headCast(ctx context.Context, key string, n int) ([]byte, error)
}

// headOf is the first castHeadProbe bytes of the cast under key.
func headOf(ctx context.Context, s Store, key string) ([]byte, error) {
	if h, ok := s.(headCaster); ok {
		return h.headCast(ctx, key, castHeadProbe)
	}
	rc, err := s.OpenCast(continuingReplay(ctx), key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, castHeadProbe))
}
