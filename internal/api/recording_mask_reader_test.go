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
