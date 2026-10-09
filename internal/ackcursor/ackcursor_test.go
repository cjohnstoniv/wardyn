// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package ackcursor

import (
	"context"
	"errors"
	"testing"
)

type row struct {
	seq  int64
	hash string
}

type fakeSource struct{ rows []row }

func (s *fakeSource) After(_ context.Context, seq int64, limit int) ([]row, error) {
	var out []row
	for _, r := range s.rows {
		if r.seq > seq && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

type fakeSink struct {
	got  [][]row
	ack  func(rows []row) int64
	fail error
}

func (s *fakeSink) Deliver(_ context.Context, rows []row) (int64, error) {
	s.got = append(s.got, rows)
	if s.fail != nil {
		return 0, s.fail
	}
	return s.ack(rows), nil
}

type memStore struct {
	pos    Pos
	getErr error
	setErr error
}

func (m *memStore) Get(context.Context) (Pos, error) { return m.pos, m.getErr }
func (m *memStore) Set(_ context.Context, p Pos) error {
	if m.setErr != nil {
		return m.setErr
	}
	m.pos = p
	return nil
}

type errSource struct{ err error }

func (s errSource) After(context.Context, int64, int) ([]row, error) { return nil, s.err }

func chain(n int) []row {
	var rs []row
	for i := 1; i <= n; i++ {
		rs = append(rs, row{int64(i), "h" + string(rune('a'+i))})
	}
	return rs
}

func ackAll(rows []row) int64 {
	if len(rows) == 0 {
		return 0
	}
	return rows[len(rows)-1].seq
}

func newCursor(src *fakeSource, sink *fakeSink, st *memStore, batch int) *Cursor[row] {
	return &Cursor[row]{Source: src, Sink: sink, Store: st, Batch: batch,
		Key: func(r row) Pos { return Pos{r.seq, r.hash} }}
}

func TestAckAdvancesDurably(t *testing.T) {
	st, sink := &memStore{}, &fakeSink{ack: ackAll}
	c := newCursor(&fakeSource{chain(3)}, sink, st, 10)
	more, err := c.Step(context.Background())
	if err != nil || more {
		t.Fatalf("Step = %v, %v", more, err)
	}
	if want := (Pos{3, "hd"}); c.Pos() != want || st.pos != want {
		t.Fatalf("pos = %v, stored %v, want %v", c.Pos(), st.pos, want)
	}
}

func TestPartialAckKeepsRestForNextStep(t *testing.T) {
	st := &memStore{}
	sink := &fakeSink{ack: func([]row) int64 { return 2 }}
	c := newCursor(&fakeSource{chain(3)}, sink, st, 10)
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.pos.Seq != 2 {
		t.Fatalf("stored seq = %d, want 2", st.pos.Seq)
	}
	sink.ack = ackAll
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sink.got[1]) != 1 || sink.got[1][0].seq != 3 {
		t.Fatalf("second batch = %v, want only seq 3", sink.got[1])
	}
}

func TestNoAckNoAdvance(t *testing.T) {
	st := &memStore{pos: Pos{1, "hb"}}
	sink := &fakeSink{fail: errors.New("down")}
	c := newCursor(&fakeSource{chain(3)}, sink, st, 10)
	_ = c.Load(context.Background())
	if _, err := c.Step(context.Background()); err == nil {
		t.Fatal("want the sink error")
	}
	if c.Pos().Seq != 1 || st.pos.Seq != 1 {
		t.Fatalf("pos = %v, stored %v, want seq 1 kept", c.Pos(), st.pos)
	}
}

func TestMismatchResetsAndResendsFromStart(t *testing.T) {
	for name, src := range map[string][]row{
		"changed hash": {{1, "ha"}, {2, "other"}, {3, "hd"}},
		"row gone":     {{1, "ha"}},
	} {
		t.Run(name, func(t *testing.T) {
			st := &memStore{pos: Pos{2, "hc"}}
			sink := &fakeSink{ack: ackAll}
			c := newCursor(&fakeSource{src}, sink, st, 10)
			var reset Pos
			c.OnReset = func(p Pos) { reset = p }
			_ = c.Load(context.Background())
			if _, err := c.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if reset != (Pos{2, "hc"}) {
				t.Fatalf("OnReset = %v", reset)
			}
			if len(sink.got[0]) == 0 || sink.got[0][0].seq != 1 {
				t.Fatalf("batch = %v, want a resend from seq 1", sink.got[0])
			}
		})
	}
}

func TestResentRowRecognisedNotRedelivered(t *testing.T) {
	st := &memStore{pos: Pos{2, "hc"}}
	sink := &fakeSink{ack: ackAll}
	c := newCursor(&fakeSource{chain(4)}, sink, st, 10)
	_ = c.Load(context.Background())
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sink.got[0]) != 2 || sink.got[0][0].seq != 3 {
		t.Fatalf("batch = %v, want seqs 3-4 only", sink.got[0])
	}
}

func TestFullBatchMoreAndEmptyBatchHeartbeat(t *testing.T) {
	st, sink := &memStore{}, &fakeSink{ack: ackAll}
	c := newCursor(&fakeSource{chain(2)}, sink, st, 2)
	if more, _ := c.Step(context.Background()); !more {
		t.Fatal("a full batch must say more")
	}
	if more, _ := c.Step(context.Background()); more || len(sink.got[1]) != 0 {
		t.Fatalf("second step more = %v batch = %v, want an empty batch", more, sink.got[1])
	}
}

func TestSinkNeverMovesCursorPastRowsRead(t *testing.T) {
	st := &memStore{}
	sink := &fakeSink{ack: func([]row) int64 { return 99 }}
	c := newCursor(&fakeSource{chain(2)}, sink, st, 10)
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.pos.Seq != 2 {
		t.Fatalf("seq = %d, want 2", st.pos.Seq)
	}
}

func TestResetIsDurableEvenWhenSinkFails(t *testing.T) {
	st := &memStore{pos: Pos{2, "hc"}}
	sink := &fakeSink{fail: errors.New("down")}
	c := newCursor(&fakeSource{[]row{{1, "ha"}}}, sink, st, 10)
	_ = c.Load(context.Background())
	if _, err := c.Step(context.Background()); err == nil {
		t.Fatal("want the sink error")
	}
	if st.pos != (Pos{}) || c.Pos() != (Pos{}) {
		t.Fatalf("stored %v, pos %v, want both reset", st.pos, c.Pos())
	}
}

func TestErrorsLeavePositionUnchanged(t *testing.T) {
	boom := errors.New("boom")
	start := Pos{1, "hb"}
	ctx := context.Background()

	t.Run("load", func(t *testing.T) {
		c := newCursor(&fakeSource{}, &fakeSink{ack: ackAll}, &memStore{pos: start, getErr: boom}, 10)
		if err := c.Load(ctx); !errors.Is(err, boom) || c.Pos() != (Pos{}) {
			t.Fatalf("Load err = %v pos = %v", err, c.Pos())
		}
	})
	t.Run("source", func(t *testing.T) {
		st := &memStore{pos: start}
		c := newCursor(&fakeSource{}, &fakeSink{ack: ackAll}, st, 10)
		c.Source = errSource{boom}
		_ = c.Load(ctx)
		if _, err := c.Step(ctx); !errors.Is(err, boom) || c.Pos() != start || st.pos != start {
			t.Fatalf("err = %v pos = %v stored = %v", err, c.Pos(), st.pos)
		}
	})
	t.Run("store set", func(t *testing.T) {
		st := &memStore{pos: start}
		c := newCursor(&fakeSource{chain(3)}, &fakeSink{ack: ackAll}, st, 10)
		_ = c.Load(ctx)
		st.setErr = boom
		if _, err := c.Step(ctx); !errors.Is(err, boom) || c.Pos() != start || st.pos != start {
			t.Fatalf("err = %v pos = %v stored = %v", err, c.Pos(), st.pos)
		}
	})
}
