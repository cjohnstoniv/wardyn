// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command wardyn-tetragon-ingest is the host-scoped eBPF GROUND-TRUTH sidecar:
// the SECOND of Wardyn's three advertised audit streams. It tails Tetragon's
// line-delimited JSON export, correlates each kernel event to a Wardyn run via
// the container label wardyn.run-id (by listing docker containers labelled
// wardyn.managed=true), maps a bounded event subset to types.AuditEvent
// (internal/groundtruth), and POSTs batches to the control plane's
// POST /api/v1/internal/groundtruth endpoint with a host-sensor bearer token.
// The control plane records them append-only — so they land in Postgres AND fan
// to every SIEM sink with ZERO new fanout code, keyed on run_id and
// discriminated by a kernel.* action prefix + data.stream="ebpf".
//
// DEPENDENCY CHOICE (documented): this binary consumes Tetragon's JSON EXPORT
// stream rather than the Tetragon gRPC client, keeping go.mod free of
// github.com/cilium/tetragon. It correlates by shelling out to `docker ps`
// rather than importing the docker client, keeping this in the default build
// with no new module dependencies (see correlator.go).
//
// HONESTY: this is DETECTION, not prevention. It is a HOST sensor that sees ALL
// containers on the host; it keeps only wardyn-managed AGENT containers and
// drops the rest. Events it cannot correlate to a run are sent with run_id NULL
// + correlation="unmapped" — never silently dropped, because blindness must be
// visible. It emits a periodic kernel.sensor.heartbeat (run_id NULL) so
// /healthz can report ebpf_groundtruth=healthy ONLY while events are arriving.
// Host eBPF is blind inside CC3/Kata guests; for such runs a one-time
// kernel.sensor.blind event is emitted (data.reason=cc3-kata-host-ebpf-blind).
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
	"github.com/cjohnstoniv/wardyn/internal/groundtruth"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("wardyn-tetragon-ingest: %v", err)
	}
}

func run() error {
	var (
		exportPath   = flagEnv("export", "WARDYN_TETRAGON_EXPORT", "/var/log/tetragon/tetragon.log", "path to the Tetragon JSON export (JSONL) to tail")
		controlURL   = flagEnv("control-plane-url", "WARDYN_CONTROL_PLANE_URL", "http://wardynd:8080", "control plane base URL")
		token        = flagEnv("token", "WARDYN_GROUNDTRUTH_TOKEN", "", "host-sensor bearer token (aud=wardyn-groundtruth); REQUIRED")
		blindRuns    = flagEnv("blind-runs", "WARDYN_GROUNDTRUTH_BLIND_RUNS", "", "comma-separated run ids the host sensor is blind to (CC3/Kata); one kernel.sensor.blind is emitted per id at boot")
		heartbeatStr = flagEnv("heartbeat", "WARDYN_GROUNDTRUTH_HEARTBEAT", "30s", "sensor heartbeat interval")
		refreshStr   = flagEnv("refresh", "WARDYN_GROUNDTRUTH_REFRESH", "15s", "container->run index refresh interval")
		statsStr     = flagEnv("stats", "WARDYN_GROUNDTRUTH_STATS", "60s", "how often to log throughput/drop stats")
		bufferSize   = flagIntEnv("buffer", "WARDYN_GROUNDTRUTH_BUFFER", 4096, "event buffer size before backpressure drops")
		batchSize    = flagIntEnv("batch", "WARDYN_GROUNDTRUTH_BATCH", 64, "max events per POST batch")
	)
	flag.Parse()

	if err := checkBootTokenSource(*token); err != nil {
		return err
	}

	heartbeatIval := mustDuration(*heartbeatStr, 30*time.Second)
	refreshIval := mustDuration(*refreshStr, 15*time.Second)
	statsIval := mustDuration(*statsStr, 60*time.Second)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client := &http.Client{Timeout: 10 * time.Second}
	sink := newEventSink(*controlURL, *token, *bufferSize, *batchSize, 2*time.Second, client)
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sink.close(shutCtx)
	}()

	// Container->run correlator. Throttle on-miss refreshes to half the ticker.
	corr := newDockerCorrelator(nil, refreshIval/2)
	if err := corr.Refresh(ctx); err != nil {
		// Non-fatal: the daemon may not be reachable yet (compose ordering).
		// Everything correlates as unmapped until the first successful refresh —
		// visible blindness, not a crash.
		log.Printf("wardyn-tetragon-ingest: initial container index refresh failed: %v (continuing; events correlate as unmapped until docker is reachable)", err)
	}
	mapper := groundtruth.NewMapper(corr)

	// One-time blindness events for CC3/Kata runs the host sensor cannot see.
	for _, rs := range splitCSV(*blindRuns) {
		runID, perr := uuid.Parse(rs)
		if perr != nil {
			log.Printf("wardyn-tetragon-ingest: skipping invalid blind run id %q: %v", rs, perr)
			continue
		}
		sink.emit(groundtruth.BlindEvent(runID, "cc3-kata-host-ebpf-blind"))
		log.Printf("wardyn-tetragon-ingest: emitted kernel.sensor.blind for run %s", runID)
	}

	// Background loops: heartbeat, index refresh, stats.
	go func() {
		// Emit one immediately so /healthz flips to healthy as soon as the
		// sensor is up and reachable, then on the interval. Each beat carries
		// the current cumulative drop count so /healthz can surface the gap
		// size. Runs in its own goroutine so this initial beat never blocks
		// startup on a control-plane POST.
		beat := func() {
			sink.emit(groundtruth.HeartbeatEventWithDropped(sink.droppedCount(), sink.observedCount()))
		}
		beat()
		every(ctx, heartbeatIval, beat)
	}()
	go every(ctx, refreshIval, func() {
		if err := corr.Refresh(ctx); err != nil {
			log.Printf("wardyn-tetragon-ingest: container index refresh failed: %v", err)
		}
	})
	go every(ctx, statsIval, func() {
		log.Printf("wardyn-tetragon-ingest: stats posted=%d dropped=%d", sink.postedCount(), sink.droppedCount())
	})

	log.Printf("wardyn-tetragon-ingest: tailing %s -> %s/api/v1/internal/groundtruth (heartbeat=%s)", *exportPath, *controlURL, heartbeatIval)

	// Tail the export. Returns on ctx cancellation.
	tailExport(ctx, *exportPath, mapper, sink)
	log.Printf("wardyn-tetragon-ingest: shutdown (posted=%d dropped=%d)", sink.postedCount(), sink.droppedCount())
	return nil
}

// checkBootTokenSource fails closed when the sensor has NO configured token
// source: an unauthenticated sensor must not start, because every POST would 401
// and the stream would produce nothing while looking alive.
//
// "Configured" means either source the sink resolves (tokenFromEnv) — the static
// env/flag token OR WARDYN_GROUNDTRUTH_TOKEN_FILE. A configured file the rotator
// has not written YET is not a boot failure: compose starts this sidecar as soon
// as wardynd reports healthy, which can precede the rotator's first write, and
// the sink re-reads the file on every 401 refresh. Until it appears, batches are
// dropped and counted — visible blindness, the same contract as an unreachable
// control plane. Rejecting that wiring at boot would kill the sidecar outright
// for a condition that heals in seconds.
func checkBootTokenSource(flagToken string) error {
	gtFile := strings.TrimSpace(os.Getenv("WARDYN_GROUNDTRUTH_TOKEN_FILE"))
	if _, err := tokenFromEnv(flagToken); err != nil && gtFile == "" {
		return fmt.Errorf("missing host-sensor token: set -token / WARDYN_GROUNDTRUTH_TOKEN or WARDYN_GROUNDTRUTH_TOKEN_FILE (aud=wardyn-groundtruth): %w", err)
	}
	if gtFile != "" {
		log.Printf("wardyn-tetragon-ingest: token source = file %s (rotatable)", gtFile)
	}
	return nil
}

// tailExport follows the JSONL export file, mapping each line and emitting the
// mapped events. It re-opens the file on truncation/rotation and waits for it to
// appear if it does not exist yet (Tetragon may start after this sidecar).
func tailExport(ctx context.Context, path string, mapper *groundtruth.Mapper, sink *eventSink) {
	var (
		f      *os.File
		reader *bufio.Reader
		// pending holds the bytes of a line not yet '\n'-terminated. ReadBytes
		// returns a partial line together with io.EOF when the writer has not
		// finished it; those bytes are already consumed from the bufio buffer, so
		// we accumulate them here and only map once a real newline arrives —
		// otherwise a line straddling an EOF boundary is split into two dropped
		// fragments (silent ground-truth loss on the tamper-proof stream).
		pending []byte
		// rotationPending is set when a rotation was detected but the new file
		// was not yet visible to reopen: the NEXT successful open must then seek
		// to START (the post-rotation file is entirely unread), closing the race
		// window instead of falling back to SeekEnd and dropping its head.
		rotationPending bool
	)
	// seekEnd=true only on the INITIAL open, so we do not replay an existing
	// large historical log on startup. A rotation reopen must seek to START:
	// the new file's beginning is unread ground-truth data — seeking to end
	// there would silently drop every event written before we noticed the
	// rotation, a security-signal loss.
	openFile := func(seekEnd bool) bool {
		nf, err := os.Open(path)
		if err != nil {
			return false
		}
		if seekEnd {
			_, _ = nf.Seek(0, io.SeekEnd)
		} else {
			_, _ = nf.Seek(0, io.SeekStart)
		}
		f = nf
		reader = bufio.NewReader(f)
		return true
	}
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()

	for ctx.Err() == nil {
		if f == nil {
			// Initial / post-read-error reopen seeks to END (don't replay an
			// existing historical log). But if a rotation was detected and its
			// new file only just became visible, seek to START so its head is
			// not skipped.
			if !openFile(!rotationPending) {
				if sleepCtx(ctx, time.Second) {
					return
				}
				continue
			}
			if rotationPending {
				log.Printf("wardyn-tetragon-ingest: reopened rotated export %s at offset 0", path)
			} else {
				log.Printf("wardyn-tetragon-ingest: opened export %s", path)
			}
			rotationPending = false
		}
		chunk, err := reader.ReadBytes('\n')
		// Reassemble lines that straddle an EOF read boundary. ReadBytes returns a
		// NON-newline-terminated partial together with io.EOF when the writer has
		// not finished the line; those bytes are already consumed from the bufio
		// buffer and can't be re-read. Processing the partial (it fails JSON parse
		// -> dropped) and later processing its remainder as a second fragment
		// silently splits and loses one kernel event on the tamper-proof stream.
		// Hold the bytes in pending; only map once ReadBytes signals a real
		// '\n'-terminated record (err == nil).
		pending = append(pending, chunk...)
		if err == nil {
			processLine(pending, mapper, sink)
			pending = pending[:0]
			continue
		}
		if err == io.EOF {
			// Detect rotation/truncation: if the file shrank, reopen at the
			// START of the new file — its beginning is unread data we must not
			// skip. If the new file is not visible yet, mark rotationPending so
			// the f==nil retry path also seeks to START once it appears (no head
			// drop), rather than falling back to SeekEnd.
			if rotated(f, path) {
				// The pending partial belongs to the old inode — genuinely gone.
				pending = pending[:0]
				_ = f.Close()
				if !openFile(false) {
					f = nil
					rotationPending = true
					if sleepCtx(ctx, 250*time.Millisecond) {
						return
					}
				} else {
					log.Printf("wardyn-tetragon-ingest: reopened rotated export %s at offset 0", path)
				}
				continue
			}
			// Not rotated: keep pending across the sleep. The writer finishes the
			// line and the next ReadBytes returns its remainder to append.
			if sleepCtx(ctx, 250*time.Millisecond) {
				return
			}
			continue
		}
		if err != nil {
			// Read error: the handle and any partial are suspect. Discard pending
			// and reopen on next loop.
			pending = pending[:0]
			_ = f.Close()
			f = nil
			if sleepCtx(ctx, time.Second) {
				return
			}
		}
	}
}

// processLine maps one export line and emits the result. Unmapped events are
// still emitted (run_id NULL) — visible blindness.
func processLine(line []byte, mapper *groundtruth.Mapper, sink *eventSink) {
	line = []byte(strings.TrimSpace(string(line)))
	if len(line) == 0 {
		return
	}
	ev, ok := mapper.MapLine(line)
	if !ok {
		// Unrecorded kind or filtered (non-sensitive) write: not an error.
		return
	}
	// Count real kernel ground-truth mapped off the tail. /healthz keys the
	// ebpf_groundtruth state off this (carried on the heartbeat): a live
	// heartbeat with observed==0 means the sensor is blind, not healthy.
	sink.markObserved()
	sink.emit(ev)
}

// rotated reports whether the file at path is a different file than the open
// handle (log rotation). Two independent signals are checked because either
// alone can miss a rotation: a size shrink catches truncate-in-place, but an
// atomic rename-and-recreate (the pattern this sidecar's own doc comment
// describes) can leave the new file's size >= our offset — in that case the
// old check would keep polling the orphaned, no-longer-written old inode
// forever while new data piles up under the same path. Comparing inode
// numbers (stable across a rename) catches that case too.
func rotated(f *os.File, path string) bool {
	cur, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return true
	}
	fi, err := os.Stat(path)
	if err != nil {
		return false // file gone momentarily; keep our handle and retry
	}
	if fi.Size() < cur {
		return true
	}
	openFI, err := f.Stat()
	if err != nil {
		return true
	}
	return inode(fi) != inode(openFI)
}

// inode extracts the inode number from a os.Stat/os.File.Stat result. Returns
// 0 (a no-op comparison) if the platform doesn't expose *syscall.Stat_t.
func inode(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}

// every calls fn on every tick of ival until ctx is cancelled.
func every(ctx context.Context, ival time.Duration, fn func()) {
	t := time.NewTicker(ival)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}

// sleepCtx sleeps for d or until ctx is done; returns true if ctx was done.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-t.C:
		return false
	}
}

// flagEnv/flagIntEnv/splitCSV are shared with cmd/wardynd via internal/cliutil
// (previously duplicated here — this file's own comment called them "mirror
// wardynd's").
var (
	flagEnv    = cliutil.FlagEnv
	flagIntEnv = cliutil.FlagIntEnv
	splitCSV   = cliutil.SplitCSV
)

func mustDuration(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(s)); err == nil {
		return d
	}
	return def
}
