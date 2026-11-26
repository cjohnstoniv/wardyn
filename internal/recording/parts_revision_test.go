// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"context"
	"io"
	"strings"
	"testing"
)

// replacingStore is an FSStore whose cast a revive replaces between the Recordings list's read of part 1 and its
// read of part 2: once the first read of part 1 returns, a new part 1 is saved. It counts every read that is not
// one revision of a part (OpenCast, StatAndTail); the list must make none.
type replacingStore struct {
	*FSStore
	runID      string
	newPart1   string
	replaced   bool
	splitReads int
}

func (r *replacingStore) statHeadTail(ctx context.Context, key string, tailBytes int64) (int64, []byte, []byte, error) {
	size, header, tail, err := r.FSStore.statHeadTail(ctx, key, tailBytes)
	if key == r.runID && !r.replaced {
		r.replaced = true
		if serr := r.FSStore.SaveCast(ctx, r.runID, strings.NewReader(r.newPart1)); serr != nil {
			return 0, nil, nil, serr
		}
	}
	return size, header, tail, err
}

func (r *replacingStore) OpenCast(ctx context.Context, key string) (io.ReadCloser, error) {
	r.splitReads++
	return r.FSStore.OpenCast(ctx, key)
}

func (r *replacingStore) StatAndTail(ctx context.Context, key string, tailBytes int64) (int64, []byte, error) {
	r.splitReads++
	return r.FSStore.StatAndTail(ctx, key, tailBytes)
}

// TestStatJoinedReadsEachPartFromOneRevision: a two-part recording A is listed while a revive replaces part 1
// with a short B1 between the list's two reads. Each part's header, size and tail come from one revision, so the
// list describes A as stored when its parts were read (both parts, A2's duration). It never pairs B1's header
// with A1's size and tail, which describes a recording replay never offered.
func TestStatJoinedReadsEachPartFromOneRevision(t *testing.T) {
	fs, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const runID = "0a000000-0000-4000-8000-0000000000a1"
	headerA := `{"version":2,"width":80,"height":24,"timestamp":1700000000}` + "\n"
	headerB := `{"version":2,"width":80,"height":24,"timestamp":1800000000}` + "\n"
	a1 := headerA + `[0.5,"o","one"]` + "\n" + `[1.0,"o","more of A"]` + "\n"
	a2 := headerA + `[2.5,"o","two"]` + "\n"
	b1 := headerB + `[0.2,"o","b"]` + "\n"
	ctx := context.Background()
	if err := fs.SaveCast(ctx, runID, strings.NewReader(a1)); err != nil {
		t.Fatal(err)
	}
	if err := fs.SaveCastNamed(ctx, runID, PartSuffix(2), strings.NewReader(a2)); err != nil {
		t.Fatal(err)
	}
	s := &replacingStore{FSStore: fs, runID: runID, newPart1: b1}
	size, tail, err := StatJoined(ctx, s, runID, 64)
	if err != nil {
		t.Fatal(err)
	}
	if !s.replaced {
		t.Fatal("part 1 was not read through statHeadTail")
	}
	if want := int64(len(a1) + len(a2)); size != want {
		t.Errorf("size = %d, want %d: A1 and A2, each from one revision", size, want)
	}
	if d, ok := LastOutputElapsed(tail); !ok || d != 2.5 {
		t.Errorf("duration = %v (ok=%v), want A2's 2.5", d, ok)
	}
	if s.splitReads != 0 {
		t.Errorf("%d separate opens or stats, want 0: a part's header, size and tail come from one read", s.splitReads)
	}
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// TestReadJoinHeaderReadsAtMostItsBound: a body with no newline for 128 KiB is read for no more than the bound
// and has no header; a short line comes back through its newline.
func TestReadJoinHeaderReadsAtMostItsBound(t *testing.T) {
	c := &countingReader{r: strings.NewReader(strings.Repeat("x", 128<<10))}
	if h, err := readJoinHeader(c); err != nil || h != nil {
		t.Fatalf("readJoinHeader = %q, %v; want nil", h[:min(len(h), 8)], err)
	}
	if c.n > maxJoinHeaderBytes+1 {
		t.Errorf("read %d bytes, want at most %d", c.n, maxJoinHeaderBytes+1)
	}
	if h, err := readJoinHeader(strings.NewReader("{}\n[0,\"o\",\"a\"]\n")); err != nil || string(h) != "{}\n" {
		t.Errorf("readJoinHeader = %q, %v; want the first line", h, err)
	}
}
