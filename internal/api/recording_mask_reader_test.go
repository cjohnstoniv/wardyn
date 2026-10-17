// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

type recordingReadProbe struct {
	io.Reader
	calls   int
	largest int
}

func (p *recordingReadProbe) Read(b []byte) (int, error) {
	p.calls++
	p.largest = max(p.largest, len(b))
	return p.Reader.Read(b)
}

func TestBuildMaskingBody_ReadsOnlyOnDemand(t *testing.T) {
	runID := uuid.New()
	reg := secretmask.NewRegistry()
	reg.Add(runID, []byte("registered-recording-secret"))
	src := &recordingReadProbe{Reader: strings.NewReader(strings.Repeat("x", 1<<20))}
	body := buildMaskingBody(src, reg, runID)
	if src.calls != 0 {
		t.Fatal("constructing the masked reader read its source")
	}
	if n, err := body.Read(nil); n != 0 || err != nil || src.calls != 0 {
		t.Fatalf("empty read = (%d, %v), source reads = %d", n, err, src.calls)
	}
	if n, err := body.Read(make([]byte, 1)); n != 1 || err != nil {
		t.Fatalf("first read = (%d, %v)", n, err)
	}
	if src.calls != 1 || src.largest > 32<<10 {
		t.Fatalf("source reads = %d, largest buffer = %d", src.calls, src.largest)
	}
	if n, err := body.Read(make([]byte, 1)); n != 1 || err != nil || src.calls != 1 {
		t.Fatalf("buffered read = (%d, %v), source reads = %d", n, err, src.calls)
	}
	// Abandoning the reader requires no Close or cleanup callback.
}

type recordingFinalRead struct {
	data string
	err  error
}

func (r *recordingFinalRead) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}

func TestBuildMaskingBody_DataAndErrorAfterMaskedTail(t *testing.T) {
	for name, terminal := range map[string]error{
		"EOF": io.EOF, "source failure": io.ErrUnexpectedEOF,
		"size cap": &http.MaxBytesError{Limit: maxRecordingUploadBytes},
	} {
		t.Run(name, func(t *testing.T) {
			runID := uuid.New()
			reg := secretmask.NewRegistry()
			reg.Add(runID, []byte("registered-recording-secret"))
			body := buildMaskingBody(&recordingFinalRead{
				data: "before registered-recording-secret tail", err: terminal,
			}, reg, runID)
			var got strings.Builder
			buf := make([]byte, 1)
			for {
				n, err := body.Read(buf)
				got.Write(buf[:n])
				if err != nil {
					if !errors.Is(err, terminal) {
						t.Fatalf("terminal error = %v, want %v", err, terminal)
					}
					break
				}
			}
			if got.String() != "before <secret-hidden> tail" {
				t.Fatalf("masked bytes before error = %q", got.String())
			}
		})
	}
}

func TestBuildMaskingBody_ByteBoundaryAndEscapedSecrets(t *testing.T) {
	runID := uuid.New()
	reg := secretmask.NewRegistry()
	secret := "first-secret-line\nsecond-secret-line"
	reg.Add(runID, []byte(secret))
	input := "raw: " + secret + `; escaped: first-secret-line\nsecond-secret-line; tail`
	body := buildMaskingBody(oneByteReader{r: strings.NewReader(input)}, reg, runID)
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "raw: <secret-hidden>; escaped: <secret-hidden>; tail" {
		t.Fatalf("masked bytewise input = %q", got)
	}
}

func TestBuildMaskingBody_EmptyRegistryPassesThrough(t *testing.T) {
	for _, reg := range []*secretmask.Registry{nil, secretmask.NewRegistry()} {
		src := strings.NewReader("unmasked without registered secrets")
		if got := buildMaskingBody(src, reg, uuid.New()); got != src {
			t.Fatal("empty registry did not return the original reader")
		}
	}
}

func TestBuildMaskingBody_MaskFailureDoesNotReturnRawInput(t *testing.T) {
	previous := secretmask.MaskCallForTest
	secretmask.MaskCallForTest = func(secretmask.Masker, []byte) []byte { panic("synthetic masker failure") }
	t.Cleanup(func() { secretmask.MaskCallForTest = previous })
	runID := uuid.New()
	reg := secretmask.NewRegistry()
	reg.Add(runID, []byte("registered-recording-secret"))
	body := buildMaskingBody(strings.NewReader("registered-recording-secret"), reg, runID)
	got, err := io.ReadAll(body)
	if err == nil || strings.Contains(string(got), "registered-recording-secret") || !strings.Contains(string(got), "<secret-hidden>") {
		t.Fatalf("mask failure returned (%q, %v)", got, err)
	}
}

// A plain log wardyn-rec cut short ends with its marker: a registered secret the cut split is masked as far as
// it arrived, at every cut offset, for a one-line and a multi-line value. A log with no marker is uploaded whole,
// so its end is left as it is.
func TestBuildMaskingBody_MasksASecretALogCutSplits(t *testing.T) {
	lines := strings.Repeat("output line\n", 4000)
	for _, secret := range []string{"cut-secret-0123456789", "-----BEGIN KEY-----\nAAAAline1\nBBBBline2\n-----END KEY-----\n"} {
		runID := uuid.New()
		reg := secretmask.NewRegistry()
		if err := reg.Add(runID, []byte(secret)); err != nil {
			t.Fatal(err)
		}
		for k := 1; k < len(secret); k++ {
			head := lines + secret[:k]
			marker := recording.LogCutMarker(int64(len(head)), int64(len(head)+len(secret)))
			got, err := io.ReadAll(buildMaskingBody(strings.NewReader(head+marker), reg, runID))
			if want := lines + string(secretmask.Placeholder()) + marker; err != nil || string(got) != want {
				t.Errorf("cut at %d of %q: %v, ends %q", k, secret, err, got[max(0, len(got)-200):])
			}
			if got, err := io.ReadAll(buildMaskingBody(strings.NewReader(head), reg, runID)); err != nil || string(got) != head {
				t.Errorf("a whole log ending at %d of %q was changed: %v", k, secret, err)
			}
		}
	}
}

// chunkedReader returns at most n bytes per Read.
type chunkedReader struct {
	r io.Reader
	n int
}

func (c chunkedReader) Read(p []byte) (int, error) { return c.r.Read(p[:min(c.n, len(p))]) }

// A secret registered inside a longer one: a log cut inside the longer value has all of it that arrived masked,
// whatever the read boundaries.
func TestBuildMaskingBody_MasksANestedSecretALogCutSplits(t *testing.T) {
	const outer, inner = "alpha-INNER1234-omega-tail", "INNER1234"
	lines := strings.Repeat("output line\n", 4000)
	runID := uuid.New()
	reg := secretmask.NewRegistry()
	for _, v := range []string{outer, inner} {
		if err := reg.Add(runID, []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	for _, chunk := range []int{1, 7, 4096} {
		for k := 1; k < len(outer); k++ {
			head := lines + outer[:k]
			marker := recording.LogCutMarker(int64(len(head)), int64(len(head)+len(outer)))
			got, err := io.ReadAll(buildMaskingBody(chunkedReader{strings.NewReader(head + marker), chunk}, reg, runID))
			if want := lines + string(secretmask.Placeholder()) + marker; err != nil || string(got) != want {
				t.Errorf("cut at %d in %d-byte reads: %v, ends %q", k, chunk, err, got[max(0, len(got)-200):])
			}
		}
	}
}

// maskedWithLimit is body through the masking reader with its masked-output limit lowered to limit.
func maskedWithLimit(t *testing.T, src io.Reader, secret string, limit int) *recordingMaskReader {
	t.Helper()
	runID := uuid.New()
	reg := secretmask.NewRegistry()
	if err := reg.Add(runID, []byte(secret)); err != nil {
		t.Fatal(err)
	}
	r := buildMaskingBody(src, reg, runID).(*recordingMaskReader)
	r.limit = limit
	return r
}

// Masking lengthens a body (a value of MinLen bytes becomes the longer placeholder), so a log within the upload
// cap can come out past the store's. The masked output is cut at the limit and ends with the server's marker, once,
// at every read size, and what comes before it is the masked log, unchanged.
func TestBuildMaskingBody_CutsAnExpandedLogAtTheStoreCap(t *testing.T) {
	const secret, limit = "expand-me", 4096
	placeholder := string(secretmask.Placeholder())
	marker := maskCutMarker(limit)
	line := "x " + secret + "\n"
	input := strings.Repeat(line, 2000) // 2000 lines, each 2+15+1 bytes masked: ~36000, far past the limit
	wantAll := strings.Repeat("x "+placeholder+"\n", 2000)
	for _, chunk := range []int{1, 7, 4096, 1 << 20} {
		r := maskedWithLimit(t, chunkedReader{strings.NewReader(input), chunk}, secret, limit)
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("%d-byte reads: %v", chunk, err)
		}
		if len(got) != limit || strings.Count(string(got), marker) != 1 || !strings.HasSuffix(string(got), marker) {
			t.Fatalf("%d-byte reads: %d bytes, marker count %d, ends %q", chunk, len(got), strings.Count(string(got), marker), got[len(got)-100:])
		}
		if keep := limit - len(marker); string(got[:keep]) != wantAll[:keep] {
			t.Fatalf("%d-byte reads: the kept bytes are not the masked log's start", chunk)
		}
		if strings.Contains(string(got), secret) {
			t.Fatalf("%d-byte reads: the secret survived", chunk)
		}
	}
}

// A masked log that fills the limit exactly is left whole; one byte more is cut.
func TestBuildMaskingBody_OnlyCutsAMaskedLogPastTheLimit(t *testing.T) {
	const secret, limit = "expand-me", 2048
	placeholder := string(secretmask.Placeholder())
	pad := func(n int) string { return strings.Repeat("p", n) }
	for _, tc := range []struct {
		name string
		size int
		cut  bool
	}{{"under", limit - 1, false}, {"exactly", limit, false}, {"one over", limit + 1, true}} {
		t.Run(tc.name, func(t *testing.T) {
			masked := pad(tc.size-len(placeholder)) + placeholder
			got, err := io.ReadAll(maskedWithLimit(t, strings.NewReader(pad(tc.size-len(placeholder))+secret), secret, limit))
			if err != nil {
				t.Fatal(err)
			}
			if tc.cut {
				if len(got) != limit || !strings.HasSuffix(string(got), maskCutMarker(limit)) {
					t.Fatalf("%d bytes, ends %q", len(got), got[len(got)-80:])
				}
			} else if string(got) != masked {
				t.Fatalf("a masked log within the limit changed: %d bytes", len(got))
			}
		})
	}
}

// The server's marker is not wardyn-rec's, so neither detector takes the other's line for its own, and a log wardyn-rec
// already cut that then expands past the limit ends with the server's marker alone and no secret.
func TestBuildMaskingBody_ServerCutMarkerIsDistinct(t *testing.T) {
	marker := maskCutMarker(4096)
	if recording.LogCutAt([]byte("log"+marker)) != -1 || recording.LogCutAt([]byte("log"+recording.LogCutMarker(3, 9))) != 3 {
		t.Fatal("the two cut markers are not told apart")
	}
	const secret, limit = "expand-me", 4096
	head := strings.Repeat("y "+secret+"\n", 1000) + secret[:4]
	body := head + recording.LogCutMarker(int64(len(head)), int64(len(head))+100)
	got, err := io.ReadAll(maskedWithLimit(t, strings.NewReader(body), secret, limit))
	if err != nil || len(got) != limit || strings.Count(string(got), marker) != 1 || strings.Contains(string(got), secret[:4]+"\n") {
		t.Fatalf("(%d bytes, %v), marker count %d", len(got), err, strings.Count(string(got), marker))
	}
}

// A cast is not cut mid-line: past the limit it comes through whole for the store to refuse, as it always was.
func TestBuildMaskingBody_DoesNotCutACast(t *testing.T) {
	const secret, limit = "expand-me", 2048
	cast := `{"version":2,"width":80,"height":24}` + "\n" + strings.Repeat(`[0.1,"o","`+secret+`"]`+"\n", 400)
	got, err := io.ReadAll(maskedWithLimit(t, strings.NewReader(cast), secret, limit))
	want := strings.ReplaceAll(cast, secret, string(secretmask.Placeholder()))
	if err != nil || string(got) != want || len(got) <= limit {
		t.Fatalf("cast changed or cut: %d bytes (want %d), %v", len(got), len(want), err)
	}
}

// At the real limit: a log that is within the upload cap before masking and past the store's after it.
func TestBuildMaskingBody_CutsAtTheRealStoreCap(t *testing.T) {
	const secret = "expand-me"
	line := strings.Repeat("a", 54) + secret + "\n" // 64 bytes in, 70 out
	src := io.LimitReader(&repeatReader{s: line}, maxRecordingUploadBytes)
	runID := uuid.New()
	reg := secretmask.NewRegistry()
	if err := reg.Add(runID, []byte(secret)); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(buildMaskingBody(src, reg, runID))
	if err != nil || len(got) != recording.MaxCastBytes || !strings.HasSuffix(string(got), maskCutMarker(recording.MaxCastBytes)) {
		t.Fatalf("(%d bytes, %v), want %d ending in the marker", len(got), err, recording.MaxCastBytes)
	}
}

// repeatReader yields s over and over.
type repeatReader struct {
	s   string
	off int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.s[r.off]
		r.off = (r.off + 1) % len(r.s)
	}
	return len(p), nil
}
