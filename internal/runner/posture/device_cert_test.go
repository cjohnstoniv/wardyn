// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package posture

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/devicecert"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

var certNow = time.Unix(1795600000, 0).UTC()

func deviceCertPeers(t *testing.T) (run *runnerwire.Peer, got chan runnerwire.DeviceCertReport) {
	t.Helper()
	got = make(chan runnerwire.DeviceCertReport, 8)
	a, b := runnerwire.Loopback()
	org := runnerwire.NewPeer(a, runnerwire.PeerConfig{Org: true, OnEventChecked: func(ev runnerwire.Event) error {
		var r runnerwire.DeviceCertReport
		if ev.Kind == runnerwire.EventDeviceCert && runnerwire.Unmarshal(ev.Data, &r) == nil {
			got <- r
		}
		return nil
	}})
	run = runnerwire.NewPeer(b, runnerwire.PeerConfig{})
	t.Cleanup(func() { org.Close(); run.Close() })
	go org.Run(t.Context()) //nolint:errcheck // the test owns shutdown
	go run.Run(t.Context()) //nolint:errcheck // the test owns shutdown
	return run, got
}

func TestDeviceCertSender_SendsAtStartAndReconnect(t *testing.T) {
	run, got := deviceCertPeers(t)
	want := runnerwire.DeviceCertReport{Proofs: []runnerwire.DeviceCertProof{{Chain: [][]byte{{1, 2}}, SignedAt: certNow, Signature: []byte{9}}}}
	var seen struct {
		dir, org string
		at       time.Time
	}
	s := newDeviceCertSender(func(_ context.Context, dir, org string, at time.Time) (runnerwire.DeviceCertReport, error) {
		seen.dir, seen.org, seen.at = dir, org, at
		return want, nil
	}, "/state", strings.Repeat("a", 64), func() time.Time { return certNow }, time.Hour)
	if err := s.Send(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	r := <-got
	if len(r.Proofs) != 1 || r.Proofs[0].Signature[0] != 9 || seen.dir != "/state" || seen.org != strings.Repeat("a", 64) || !seen.at.Equal(certNow) {
		t.Fatalf("report %+v, helper saw %+v", r, seen)
	}
	// A reconnect is a new session: it presents again.
	again, gotAgain := deviceCertPeers(t)
	if err := s.Send(t.Context(), again); err != nil {
		t.Fatal(err)
	}
	if r := <-gotAgain; len(r.Proofs) != 1 {
		t.Fatalf("reconnect report %+v", r)
	}
}

func TestDeviceCert_AHelperFailureSendsNothing(t *testing.T) {
	run, got := deviceCertPeers(t)
	s := newDeviceCertSender(func(context.Context, string, string, time.Time) (runnerwire.DeviceCertReport, error) {
		return runnerwire.DeviceCertReport{}, errors.New("no keychain")
	}, "/state", strings.Repeat("a", 64), time.Now, time.Hour)
	if err := s.Send(t.Context(), run); err == nil {
		t.Fatal("want the helper's error")
	}
	select {
	case r := <-got:
		t.Fatalf("sent %+v after a helper failure", r)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestDeviceCert_WatchPresentsAgainEveryInterval(t *testing.T) {
	run, got := deviceCertPeers(t)
	var n atomic.Int32
	s := newDeviceCertSender(func(context.Context, string, string, time.Time) (runnerwire.DeviceCertReport, error) {
		n.Add(1)
		return runnerwire.DeviceCertReport{Proofs: []runnerwire.DeviceCertProof{}}, nil
	}, "/state", strings.Repeat("a", 64), time.Now, 5*time.Millisecond)
	go s.Watch(run)
	go s.Watch(run) // a second watcher of the same session does nothing
	for i := 0; i < 3; i++ {
		select {
		case <-got:
		case <-time.After(5 * time.Second):
			t.Fatalf("report %d never arrived", i)
		}
	}
}

func TestDeviceCertSender_NoHelperSendsNothing(t *testing.T) {
	if s := NewDeviceCertSender(filepath.Join(t.TempDir(), "wardyn-macvm"), "/state", strings.Repeat("a", 64)); s != nil {
		t.Fatal("a sender with no helper beside it")
	}
	if s := NewDeviceCertSender(t.TempDir(), "/state", strings.Repeat("a", 64)); s != nil {
		t.Fatal("a sender whose helper is a directory")
	}
}

func fakeHelper(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a shell script helper")
	}
	p := filepath.Join(t.TempDir(), "wardyn-macvm")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDeviceCert_ExecHelperPassesTypedFieldsAndReadsTheReport(t *testing.T) {
	out := filepath.Join(t.TempDir(), "args")
	p := fakeHelper(t, `echo "$@" > `+out+`
echo '{"proofs":[{"chain":["AQI="],"signed_at":"`+certNow.Format(time.RFC3339)+`","signature":"CQ=="}]}'
`)
	rep, err := execHelper(p)(t.Context(), "/state", strings.Repeat("b", 64), certNow)
	if err != nil || len(rep.Proofs) != 1 || rep.Proofs[0].Signature[0] != 9 {
		t.Fatalf("report %+v, %v", rep, err)
	}
	args, _ := os.ReadFile(out)
	if got, want := strings.TrimSpace(string(args)), "device-identity --state-dir /state --org-sha256 "+strings.Repeat("b", 64)+" --signed-at "+certNow.Format(time.RFC3339); got != want {
		t.Fatalf("args %q, want %q", got, want)
	}
}

func TestDeviceCert_ExecHelperRefusesABadRun(t *testing.T) {
	for name, script := range map[string]string{
		"an exit code":     "echo 'wardyn-macvm: no keychain' >&2\nexit 2\n",
		"not json":         "echo hello\n",
		"an unknown field": `echo '{"proofs":[],"extra":1}'` + "\n",
		"too much output":  "head -c 2000000 /dev/zero\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := execHelper(fakeHelper(t, script))(t.Context(), "/state", strings.Repeat("b", 64), certNow); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

// A refused ACK (an organisation that predates the event, or one that refuses it for good) ends the sending for
// that session, and the next session asks again.
func TestDeviceCertSender_RefusedAckStops(t *testing.T) {
	var calls atomic.Int32
	s := newDeviceCertSender(func(context.Context, string, string, time.Time) (runnerwire.DeviceCertReport, error) {
		calls.Add(1)
		return runnerwire.DeviceCertReport{}, nil
	}, "/state", strings.Repeat("a", 64), time.Now, time.Hour)
	var acks atomic.Int32
	s.ack = func(context.Context, *runnerwire.Peer, runnerwire.Event) error {
		acks.Add(1)
		return &runnerwire.EventRefusal{Code: runnerwire.RefusalUnsupportedKind, Err: errors.New("unknown kind")}
	}
	first, _ := deviceCertPeers(t)
	for i := 0; i < 3; i++ {
		if err := s.Send(t.Context(), first); err != nil {
			t.Fatal(err)
		}
	}
	if acks.Load() != 1 || calls.Load() != 1 {
		t.Fatalf("%d acks and %d helper runs on one refused session, want 1 and 1", acks.Load(), calls.Load())
	}
	second, _ := deviceCertPeers(t)
	if err := s.Send(t.Context(), second); err != nil || acks.Load() != 2 {
		t.Fatalf("the next session sent %d times (%v), want a second ask", acks.Load(), err)
	}
}

// Watch presents at once, not after the first interval: it is the only sender, so READY never waits on the helper.
func TestDeviceCertSender_WatchSendsAtOnce(t *testing.T) {
	run, got := deviceCertPeers(t)
	s := newDeviceCertSender(func(context.Context, string, string, time.Time) (runnerwire.DeviceCertReport, error) {
		return runnerwire.DeviceCertReport{Proofs: []runnerwire.DeviceCertProof{}}, nil
	}, "/state", strings.Repeat("a", 64), time.Now, time.Hour)
	go s.Watch(run)
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the first report waited for the interval")
	}
}

// A report over the organisation's event cap is trimmed, whole identities at a time and the largest chain first,
// never sent oversize; and the sending goes on.
func TestDeviceCertSender_TrimsAnOversizeReportAndKeepsSending(t *testing.T) {
	big := func(n int, b byte) runnerwire.DeviceCertProof {
		return runnerwire.DeviceCertProof{Chain: [][]byte{bytes.Repeat([]byte{b}, n)}, SignedAt: certNow, Signature: []byte{b}}
	}
	// Eight chains of about 8 KiB (about 11 KiB each as base64, 88 KiB in all) and one of 100 bytes.
	var proofs []runnerwire.DeviceCertProof
	for i := 0; i < 8; i++ {
		proofs = append(proofs, big(devicecert.MaxDER-i, byte(i+1)))
	}
	proofs = append(proofs, big(100, 99))
	run, got := deviceCertPeers(t)
	var sizes []int
	s := newDeviceCertSender(func(context.Context, string, string, time.Time) (runnerwire.DeviceCertReport, error) {
		return runnerwire.DeviceCertReport{Proofs: proofs}, nil
	}, "/state", strings.Repeat("a", 64), time.Now, time.Hour)
	s.ack = func(ctx context.Context, p *runnerwire.Peer, ev runnerwire.Event) error {
		sizes = append(sizes, len(ev.Data))
		return p.EventAck(ctx, ev)
	}
	if err := s.Send(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	r := <-got
	if len(sizes) != 1 || sizes[0] > devicecert.MaxEventBytes {
		t.Fatalf("sent sizes %v, want one event within %d", sizes, devicecert.MaxEventBytes)
	}
	var kept []byte
	for _, p := range r.Proofs {
		kept = append(kept, p.Signature[0])
	}
	// The three largest chains (the first three) went, one whole identity at a time; the rest stayed in order.
	if want := []byte{4, 5, 6, 7, 8, 99}; !bytes.Equal(kept, want) {
		t.Fatalf("kept identities %v, want %v", kept, want)
	}
	// Still sending: a second Send on the same session goes out.
	if err := s.Send(t.Context(), run); err != nil || len(sizes) != 2 {
		t.Fatalf("second send: %v, %d events", err, len(sizes))
	}
	<-got
}

// A helper that skipped an identity says so on stderr and still exits 0: the runner logs it and uses the report.
func TestDeviceCert_ExecHelperLogsItsNotesOnSuccess(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(old) })
	p := fakeHelper(t, `echo 'wardyn-macvm: skipped the identity whose certificate has SHA-256 abc: refused' >&2
echo '{"proofs":[]}'
`)
	rep, err := execHelper(p)(t.Context(), "/state", strings.Repeat("b", 64), certNow)
	if err != nil || len(rep.Proofs) != 0 {
		t.Fatalf("report %+v, %v", rep, err)
	}
	if got := logs.String(); !strings.Contains(got, "level=INFO") || !strings.Contains(got, "skipped the identity whose certificate has SHA-256 abc") {
		t.Fatalf("log %q does not carry the helper's note at Info", got)
	}
}

func reportSize(t *testing.T, proofs []runnerwire.DeviceCertProof) int {
	t.Helper()
	b, err := runnerwire.Marshal(runnerwire.DeviceCertReport{Proofs: proofs})
	if err != nil {
		t.Fatal(err)
	}
	return len(b)
}

func proofOf(n int, b byte) runnerwire.DeviceCertProof {
	return runnerwire.DeviceCertProof{Chain: [][]byte{bytes.Repeat([]byte{b}, n)}, SignedAt: certNow, Signature: []byte{b}}
}

// withSize is a large identity that makes its report, beside small, exactly target bytes: the chain's length steps the
// size by 4 and the signing time's fraction by 2 to 5, so some pair lands on any target.
func withSize(t *testing.T, small runnerwire.DeviceCertProof, target int) runnerwire.DeviceCertProof {
	t.Helper()
	for d := 0; d <= 4; d++ {
		at := certNow
		if d > 0 {
			at = certNow.Add(time.Duration(int(1e9) / pow10(d))) // a fraction of d digits
		}
		build := func(n int) runnerwire.DeviceCertProof {
			big := proofOf(n, 3)
			big.SignedAt = at
			return big
		}
		size := func(n int) int { return reportSize(t, []runnerwire.DeviceCertProof{small, build(n)}) }
		lo, hi := 0, target // the smallest n whose report reaches the target
		for lo < hi {
			if mid := (lo + hi) / 2; size(mid) >= target {
				hi = mid
			} else {
				lo = mid + 1
			}
		}
		for n := lo; n < lo+3; n++ {
			if size(n) == target {
				return build(n)
			}
		}
	}
	t.Fatalf("no identity makes a report of %d bytes", target)
	return runnerwire.DeviceCertProof{}
}

func pow10(d int) int {
	n := 1
	for ; d > 0; d-- {
		n *= 10
	}
	return n
}

// A lone identity over the cap leaves an empty report, which still goes out; a report of exactly the cap goes out
// whole, and one byte more loses an identity.
func TestMarshalWithinCap_LoneIdentityAndExactBoundary(t *testing.T) {
	data, err := marshalWithinCap(t.Context(), runnerwire.DeviceCertReport{Proofs: []runnerwire.DeviceCertProof{proofOf(devicecert.MaxEventBytes, 1)}})
	var rep runnerwire.DeviceCertReport
	if err != nil || len(data) > devicecert.MaxEventBytes || runnerwire.Unmarshal(data, &rep) != nil || len(rep.Proofs) != 0 {
		t.Fatalf("lone oversize identity: %d bytes, %+v, %v", len(data), rep, err)
	}
	small := proofOf(10, 2)
	for _, c := range []struct {
		size, proofs int
	}{{devicecert.MaxEventBytes, 2}, {devicecert.MaxEventBytes + 1, 1}} {
		big := withSize(t, small, c.size)
		data, err := marshalWithinCap(t.Context(), runnerwire.DeviceCertReport{Proofs: []runnerwire.DeviceCertProof{small, big}})
		rep := runnerwire.DeviceCertReport{}
		if err != nil || runnerwire.Unmarshal(data, &rep) != nil || len(rep.Proofs) != c.proofs {
			t.Fatalf("a report of %d bytes kept %d proofs (%v), want %d", c.size, len(rep.Proofs), err, c.proofs)
		}
		if c.proofs == 2 && len(data) != devicecert.MaxEventBytes {
			t.Fatalf("the report sent is %d bytes, want exactly %d", len(data), devicecert.MaxEventBytes)
		}
		if c.proofs == 1 && (len(data) > devicecert.MaxEventBytes || rep.Proofs[0].Signature[0] != 2) {
			t.Fatalf("over the cap by one: %d bytes, kept %v", len(data), rep.Proofs[0].Signature)
		}
	}
}

// The helper's context ends with the session: a Watch whose helper is waiting returns when the peer ends.
func TestDeviceCertSender_WatchEndsWithThePeer(t *testing.T) {
	run, _ := deviceCertPeers(t)
	started := make(chan struct{})
	s := newDeviceCertSender(func(ctx context.Context, _, _ string, _ time.Time) (runnerwire.DeviceCertReport, error) {
		close(started)
		<-ctx.Done()
		return runnerwire.DeviceCertReport{}, ctx.Err()
	}, "/state", strings.Repeat("a", 64), time.Now, time.Hour)
	done := make(chan struct{})
	go func() { s.Watch(run); close(done) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the helper never started")
	}
	run.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Watch kept waiting on the helper after the peer ended")
	}
}
