// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

func TestCopyOutput_JoinedDecodedPayloads(t *testing.T) {
	s, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	saveParts(t, s, "decode", partHeader+`[0,"o","first\r\n\"escaped\" "]`+"\n"+`[1,"i","private input"]`+"\n",
		partHeader+`[2,"r","100x30"]`+"\n"+`[3,"o","snowman \u2603\u0000end"]`+"\n")
	if err := s.SaveCastNamed(t.Context(), "decode", "attach-session", strings.NewReader(partHeader+partEv3)); err != nil {
		t.Fatal(err)
	}
	r, err := recording.OpenJoined(t.Context(), s, "decode")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var got bytes.Buffer
	if err := recording.CopyOutput(t.Context(), &got, r); err != nil {
		t.Fatal(err)
	}
	if want := "first\r\n\"escaped\" snowman ☃\x00end"; got.String() != want {
		t.Fatalf("decoded %q, want %q", got.String(), want)
	}
}

func TestCopyOutput_InvalidCastNeverIncludesContentInError(t *testing.T) {
	for _, cast := range []string{
		"", "private plaintext log\n", `{"version":1,"width":80,"height":24}`,
		`{"version":2,"width":0,"height":24}`, partHeader + `[0,"o","private-secret"`,
		partHeader + `[null,"o","private-secret"]`, partHeader + `[-1,"o","private-secret"]`,
		partHeader + `[0,"o",null]`, partHeader + `[0,"o",42]`, partHeader + `[0,"","private-secret"]`,
		partHeader + `[0,"o","private-secret","extra"]`,
	} {
		var dst bytes.Buffer
		err := recording.CopyOutput(t.Context(), &dst, strings.NewReader(cast))
		if !errors.Is(err, recording.ErrInvalidCast) || strings.Contains(err.Error(), "private") {
			t.Errorf("invalid cast error %v, want content-free ErrInvalidCast", err)
		}
	}
}

func TestCopyOutput_EmptyAndCanceled(t *testing.T) {
	for _, cast := range []string{partHeader, partHeader + `[0,"i","input only"]` + "\n"} {
		var dst bytes.Buffer
		if err := recording.CopyOutput(t.Context(), &dst, strings.NewReader(cast)); err != nil || dst.Len() != 0 {
			t.Fatalf("empty output = %q, %v", dst.String(), err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := recording.CopyOutput(ctx, io.Discard, strings.NewReader(partHeader)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled decode: %v", err)
	}
}

type endlessCastHeader struct {
	read   int
	closed bool
}

func (r *endlessCastHeader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	r.read += len(p)
	return len(p), nil
}

func (r *endlessCastHeader) Close() error { r.closed = true; return nil }

type oversizedCastStore struct {
	recording.Store
	reader *endlessCastHeader
}

func (s oversizedCastStore) OpenCast(context.Context, string) (io.ReadCloser, error) {
	return s.reader, nil
}

func TestCopyOutput_AndJoinBoundUnterminatedLines(t *testing.T) {
	header := &endlessCastHeader{}
	if rc, err := recording.OpenJoined(t.Context(), oversizedCastStore{reader: header}, "oversized"); !errors.Is(err, recording.ErrInvalidCast) || rc != nil || !header.closed {
		t.Fatalf("unbounded header: reader=%v err=%v closed=%v", rc, err, header.closed)
	}
	if header.read > (64<<20)+(32<<10) {
		t.Fatalf("read %d header bytes past existing part cap", header.read)
	}
	event := &endlessCastHeader{}
	err := recording.CopyOutput(t.Context(), io.Discard, io.MultiReader(strings.NewReader(partHeader+`[0,"o","`), event))
	if !errors.Is(err, recording.ErrInvalidCast) || event.read > (64<<20)+(32<<10) {
		t.Fatalf("unbounded event: read=%d err=%v", event.read, err)
	}
}
