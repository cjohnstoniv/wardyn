// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

// A secret registered for the run after its upload's reader was built, while the upload waited in the store's
// queue, is masked: with no secret at all when the reader was built, and beside one it already held.
func TestBuildMaskingBody_MasksASecretRegisteredAfterConstruction(t *testing.T) {
	for name, older := range map[string]string{"empty corpus": "", "older secret": "older-recording-secret"} {
		t.Run(name, func(t *testing.T) {
			runID := uuid.New()
			reg := secretmask.NewRegistry()
			input := "a late-recording-secret b"
			want := "a <secret-hidden> b"
			if older != "" {
				if err := reg.Add(runID, []byte(older)); err != nil {
					t.Fatal(err)
				}
				input, want = input+" "+older, want+" <secret-hidden>"
			}
			body := buildMaskingBody(strings.NewReader(input), reg, runID)
			if err := reg.Add(runID, []byte("late-recording-secret")); err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(body)
			if err != nil || string(got) != want {
				t.Fatalf("masked = (%q, %v), want %q", got, err, want)
			}
		})
	}
}

// enteredReader signals each Read before it blocks on the source.
type enteredReader struct {
	io.Reader
	entered chan struct{}
}

func (e enteredReader) Read(p []byte) (int, error) {
	e.entered <- struct{}{}
	return e.Reader.Read(p)
}

// A secret registered while the reader is blocked reading the body is masked in the bytes that come after.
func TestBuildMaskingBody_MasksASecretRegisteredDuringABlockedRead(t *testing.T) {
	runID := uuid.New()
	reg := secretmask.NewRegistry()
	pr, pw := io.Pipe()
	src := enteredReader{Reader: pr, entered: make(chan struct{}, 64)}
	body := buildMaskingBody(src, reg, runID)
	type result struct {
		got []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		got, err := io.ReadAll(body)
		done <- result{got, err}
	}()
	<-src.entered
	if _, err := pw.Write([]byte("start ")); err != nil {
		t.Fatal(err)
	}
	<-src.entered // blocked on the pipe for the next bytes
	if err := reg.Add(runID, []byte("pipe-recording-secret")); err != nil {
		t.Fatal(err)
	}
	// More follows the secret than the reader holds back, so the secret is masked before the body ends.
	after := strings.Repeat("e", 2*recording.LogCutMarkerMax)
	if _, err := pw.Write([]byte("pipe-recording-secret " + after)); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	res := <-done
	if res.err != nil || string(res.got) != "start <secret-hidden> "+after {
		t.Fatalf("masked = (%q, %v)", res.got, res.err)
	}
}

// hookReader returns its chunks in order, a chunk over several Reads when it is longer than the buffer, and runs
// before[i] just before the first Read of chunk i returns.
type hookReader struct {
	chunks []string
	before map[int]func()
	next   int
	rest   string
}

func (h *hookReader) Read(p []byte) (int, error) {
	if h.rest == "" {
		if h.next == len(h.chunks) {
			return 0, io.EOF
		}
		if f := h.before[h.next]; f != nil {
			f()
		}
		h.rest = h.chunks[h.next]
		h.next++
	}
	n := copy(p, h.rest)
	h.rest = h.rest[n:]
	return n, nil
}

// A secret registered between two chunks of the body is masked in the later one, before the body ends: more
// follows it than the reader holds back.
func TestBuildMaskingBody_MasksASecretRegisteredBetweenChunks(t *testing.T) {
	runID := uuid.New()
	reg := secretmask.NewRegistry()
	lines := strings.Repeat("output line\n", 8000) // past the reader's held bytes and the masker's tail
	after := strings.Repeat("y", 2*recording.LogCutMarkerMax)
	src := &hookReader{
		chunks: []string{lines, "x between-recording-secret " + after},
		before: map[int]func(){1: func() {
			if err := reg.Add(runID, []byte("between-recording-secret")); err != nil {
				t.Error(err)
			}
		}},
	}
	got, err := io.ReadAll(buildMaskingBody(src, reg, runID))
	if want := lines + "x <secret-hidden> " + after; err != nil || string(got) != want {
		t.Fatalf("masked = (%v), ends %q", err, got[max(0, len(got)-60):])
	}
}

// A newly registered value's JSON-escaped rendering, as an asciicast "o" event carries it, is masked.
func TestBuildMaskingBody_MasksTheEscapedRenderingOfANewSecret(t *testing.T) {
	runID := uuid.New()
	reg := secretmask.NewRegistry()
	body := buildMaskingBody(strings.NewReader(`[0.5,"o","multi\nline-recording-secret"]`+"\n"), reg, runID)
	if err := reg.Add(runID, []byte("multi\nline-recording-secret")); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(body)
	if want := `[0.5,"o","<secret-hidden>"]` + "\n"; err != nil || string(got) != want {
		t.Fatalf("masked = (%q, %v), want %q", got, err, want)
	}
}

// The held bytes, kept back to learn whether the body ends with wardyn-rec's log cut marker, are flushed through
// the corpus as it stands at the end, not as it stood when they were read: a secret the cut split is masked.
func TestBuildMaskingBody_HeldTailUsesTheFreshCorpus(t *testing.T) {
	runID := uuid.New()
	reg := secretmask.NewRegistry()
	const secret = "late-cut-recording-secret"
	head := strings.Repeat("output line\n", 4000) + secret[:10]
	marker := recording.LogCutMarker(int64(len(head)), int64(len(head)+len(secret)))
	src := &hookReader{
		chunks: []string{head, marker}, // the start of the secret is held, unmasked, when it is registered
		before: map[int]func(){1: func() {
			if err := reg.Add(runID, []byte(secret)); err != nil {
				t.Error(err)
			}
		}},
	}
	got, err := io.ReadAll(buildMaskingBody(src, reg, runID))
	if want := head[:len(head)-10] + string(secretmask.Placeholder()) + marker; err != nil || string(got) != want {
		t.Fatalf("masked = (%v), ends %q", err, got[max(0, len(got)-200):])
	}
}

// freshBackend is a shared corpus whose Fresh answers err and records each arrival it was asked about. Only
// Fresh is called on it here.
type freshBackend struct {
	secretmask.Backend
	mu       sync.Mutex
	err      func(call int) error
	arrivals []time.Time
}

func (b *freshBackend) Fresh(_ context.Context, arrived time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.arrivals = append(b.arrivals, arrived)
	return b.err(len(b.arrivals))
}

// The shared corpus is read before the masker is: a value another server committed before the bytes arrived,
// which only that read brings into this server's registry, is masked in the first batch, mid-stream.
func TestBuildMaskingBody_SharedCorpusIsReadBeforeTheMasker(t *testing.T) {
	runID := uuid.New()
	reg := secretmask.NewRegistry()
	const secret = "replica-recording-secret"
	reg.SetBackend(&freshBackend{err: func(call int) error {
		if call == 1 {
			reg.AddLocal(runID, []byte(secret))
		}
		return nil
	}})
	rest := strings.Repeat("x", freshBatch) // the first batch ends long after the secret
	got, err := io.ReadAll(buildMaskingBody(strings.NewReader(secret+rest), reg, runID))
	if err != nil || string(got) != "<secret-hidden>"+rest {
		t.Fatalf("masked = (%v), starts %q", err, got[:min(len(got), 60)])
	}
}

// lastReadReader records when its last Read returned.
type lastReadReader struct {
	io.Reader
	returned time.Time
	reads    int
}

func (l *lastReadReader) Read(p []byte) (int, error) {
	n, err := l.Reader.Read(p)
	l.reads++
	l.returned = time.Now()
	return n, err
}

// With a shared corpus, a batch of the body is masked only after a corpus read that began after its last byte
// arrived, and one proof covers a batch, not each read.
func TestBuildMaskingBody_SharedCorpusIsReadAfterTheBytesArrive(t *testing.T) {
	runID := uuid.New()
	reg := secretmask.NewRegistry()
	src := &lastReadReader{Reader: strings.NewReader(strings.Repeat("x", 3*freshBatch))}
	var b *freshBackend
	b = &freshBackend{err: func(int) error {
		if b.arrivals[len(b.arrivals)-1].Before(src.returned) {
			return errors.New("asked about bytes that arrived later")
		}
		return nil
	}}
	reg.SetBackend(b)
	got, err := io.ReadAll(buildMaskingBody(src, reg, runID))
	if err != nil || len(got) != 3*freshBatch {
		t.Fatalf("read = (%d bytes, %v)", len(got), err)
	}
	if n := len(b.arrivals); n == 0 || n > 4 || n >= src.reads {
		t.Fatalf("freshness proofs = %d for %d reads, want one per batch", n, src.reads)
	}
}

// A corpus that cannot be proven fresh ends the upload with an error, whether at its first batch or a later one:
// the store gets the error, keeps nothing, and the route answers 503 as for an uncovered run. This is the shared
// registry's freshness, apart from the manifest's fence (TestRecordingMaskReader_GuardEndsTheUpload).
func TestBuildMaskingBody_StaleSharedCorpusFailsTheUpload(t *testing.T) {
	for name, failAt := range map[string]int{"first batch": 1, "later batch": 2} {
		t.Run(name, func(t *testing.T) {
			runID := uuid.New()
			reg := secretmask.NewRegistry()
			reg.SetBackend(&freshBackend{err: func(call int) error {
				if call >= failAt {
					return errors.New("postgres does not answer")
				}
				return nil
			}})
			store, err := recording.NewFSStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			body := buildMaskingBody(strings.NewReader(strings.Repeat("output line\n", freshBatch/4)), reg, runID)
			err = store.SaveCast(context.Background(), runID.String(), body)
			if !errors.Is(err, errMaskStale) || !errors.Is(err, errMaskUncovered) {
				t.Fatalf("save = %v, want the stale corpus error", err)
			}
			if rc, oerr := store.OpenCast(context.Background(), runID.String()); oerr == nil {
				rc.Close()
				t.Fatal("a cast was stored from an upload whose corpus was stale")
			}
		})
	}
}

// scriptReader answers each Read with its next step, which may act before it returns: a source that changes
// the registry after the reader's guard ran and before the bytes come back.
type scriptReader struct {
	steps []func() (string, error)
	next  int
}

func (s *scriptReader) Read(p []byte) (int, error) {
	if s.next == len(s.steps) {
		return 0, io.EOF
	}
	data, err := s.steps[s.next]()
	s.next++
	return copy(p, data), err
}

// maskErasure is a mask_copies erase as an upload sees it: the run's manifest fenced, so the guard answers false,
// and the run's value dropped from the registry.
type maskErasure struct {
	reg    *secretmask.Registry
	runID  uuid.UUID
	value  []byte
	fenced atomic.Bool
}

func newMaskErasure(t *testing.T, reg *secretmask.Registry, runID uuid.UUID, value string) *maskErasure {
	t.Helper()
	if !reg.AddLocal(runID, []byte(value)) {
		t.Fatal("the value was not registered")
	}
	return &maskErasure{reg: reg, runID: runID, value: []byte(value)}
}

func (e *maskErasure) apply() {
	e.fenced.Store(true)
	e.reg.DropRunValue(e.runID, e.value)
}

func (e *maskErasure) covered() bool { return !e.fenced.Load() }

// saveUncovered saves body through a real file store and requires the save to fail as uncovered, not as a stale
// corpus, with nothing stored.
func saveUncovered(t *testing.T, runID uuid.UUID, save func(*recording.FSStore) error) {
	t.Helper()
	store, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := save(store); !errors.Is(err, errMaskUncovered) || errors.Is(err, errMaskStale) {
		t.Fatalf("save = %v, want errMaskUncovered", err)
	}
	if rc, err := store.OpenCast(context.Background(), runID.String()); err == nil {
		rc.Close()
		t.Fatal("a cast was stored after the run's masking value was erased")
	}
}

// An erasure the last shared-corpus read brings in, dropping the run's only value and fencing the run, ends the
// upload as uncovered: the smaller corpus is not adopted on the guard's answer from before the read.
func TestBuildMaskingBody_ErasureAtTheLastFreshReadFailsTheUpload(t *testing.T) {
	runID := uuid.New()
	reg := secretmask.NewRegistry()
	const secret = "erased-recording-secret"
	e := newMaskErasure(t, reg, runID, secret)
	reg.SetBackend(&freshBackend{err: func(call int) error {
		if call == 2 { // the read for the final batch, its end of body included
			e.apply()
		}
		return nil
	}})
	body := buildGuardedMaskingBody(strings.NewReader(strings.Repeat("x", freshBatch)+" "+secret+" tail"), reg, runID, e.covered)
	saveUncovered(t, runID, func(s *recording.FSStore) error {
		return s.SaveCast(context.Background(), runID.String(), body)
	})
}

// The same erasure landing while the source read is blocked, its bytes and the end of the body coming back
// after it, ends the upload as uncovered.
func TestBuildMaskingBody_ErasureDuringABlockedReadFailsTheUpload(t *testing.T) {
	runID := uuid.New()
	reg := secretmask.NewRegistry()
	const secret = "erased-recording-secret"
	e := newMaskErasure(t, reg, runID, secret)
	entered, gate := make(chan struct{}), make(chan struct{})
	src := &scriptReader{steps: []func() (string, error){
		func() (string, error) { return "start ", nil },
		func() (string, error) {
			close(entered)
			<-gate
			return secret + " tail", io.EOF
		},
	}}
	body := buildGuardedMaskingBody(src, reg, runID, e.covered)
	saveUncovered(t, runID, func(s *recording.FSStore) error {
		done := make(chan error, 1)
		go func() { done <- s.SaveCast(context.Background(), runID.String(), body) }()
		<-entered
		e.apply()
		close(gate)
		return <-done
	})
}

// An erasure that lands in the read that finds the end of the body, while the secret is still in the bytes held
// back for the log cut marker, ends the upload as uncovered: the held bytes are not flushed with the smaller
// corpus.
func TestBuildMaskingBody_ErasureAtTheEndFailsTheHeldTail(t *testing.T) {
	runID := uuid.New()
	reg := secretmask.NewRegistry()
	const secret = "erased-recording-secret"
	e := newMaskErasure(t, reg, runID, secret)
	src := &scriptReader{steps: []func() (string, error){
		func() (string, error) { return strings.Repeat("output line\n", 40) + secret, nil },
		func() (string, error) {
			e.apply()
			return "", io.EOF
		},
	}}
	body := buildGuardedMaskingBody(src, reg, runID, e.covered)
	saveUncovered(t, runID, func(s *recording.FSStore) error {
		return s.SaveCast(context.Background(), runID.String(), body)
	})
}

// A value the coverage check itself loads into the registry (Covered loads a manifest's values) moves the
// generation after the masker was read, so the masker is read again before it is adopted.
func TestBuildMaskingBody_ValueTheGuardLoadsIsMasked(t *testing.T) {
	runID := uuid.New()
	reg := secretmask.NewRegistry()
	const secret = "manifest-recording-secret"
	calls := 0
	guard := func() bool {
		calls++
		if calls == 2 { // the first check after the masker was read; the first is fill's, before the read
			reg.AddLocal(runID, []byte(secret))
		}
		return true
	}
	after := strings.Repeat("b", 2*recording.LogCutMarkerMax)
	got, err := io.ReadAll(buildGuardedMaskingBody(strings.NewReader("a "+secret+" "+after), reg, runID, guard))
	if want := "a <secret-hidden> " + after; err != nil || string(got) != want {
		t.Fatalf("masked = (%v), starts %q", err, got[:min(len(got), 60)])
	}
}
