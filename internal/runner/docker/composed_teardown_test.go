// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/offline"
	"github.com/cjohnstoniv/wardyn/internal/runner/runneragent"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The teardown answers of a real Driver, read by the callers that act on them: runneragent's dispatcher, the
// offline manager's queued end, and runner.SplitAWSCacheWipe, the org's reading of an end.

// mutations counts what a dispatcher persisted: AfterRefCall runs only for a call it answers done, and
// MutationFinished runs for every call with its cause.
type mutations struct {
	mu     sync.Mutex
	after  int
	causes []error
}

func (c *mutations) counts() (after int, causes []error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.after, append([]error(nil), c.causes...)
}

// composedDispatcher is runneragent's dispatcher over sub, holding every ref, with lifecycle hooks that
// record what it persisted. prefix is the runner's ref prefix the org names refs with.
func composedDispatcher(t *testing.T, sub *Driver) (d *runneragent.Dispatcher, prefix string, c *mutations) {
	t.Helper()
	id := uuid.NewString()
	c = &mutations{}
	d, err := runneragent.New(runneragent.Config{RunnerID: id, Substrate: sub,
		HoldsSandbox:  func(string) bool { return true },
		BeforeCreate:  func(context.Context, runner.SandboxSpec) error { return nil },
		Created:       func(context.Context, runner.SandboxSpec, string) error { return nil },
		CreateFailed:  func(context.Context, runner.SandboxSpec) error { return nil },
		BeforeRefCall: func(context.Context, string, string, uuid.UUID) error { return nil },
		AfterRefCall: func(context.Context, string, string, string, uuid.UUID) error {
			c.mu.Lock()
			c.after++
			c.mu.Unlock()
			return nil
		},
		MutationFinished: func(_ context.Context, _, _ string, _, _ uuid.UUID, cause error) error {
			c.mu.Lock()
			c.causes = append(c.causes, cause)
			c.mu.Unlock()
			return nil
		},
		ExecutionResult: func(context.Context, string, uuid.UUID) (runnerwire.ExecutionResult, error) {
			return runnerwire.ExecutionResult{}, errors.New("no execution in this test")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return d, placement.RunnerRefPrefix(id), c
}

// onlinePeer is a live session's peer: the dispatcher refuses every call on a link that is down.
func onlinePeer(t *testing.T) *runnerwire.Peer {
	t.Helper()
	a, b := runnerwire.Loopback()
	p := runnerwire.NewPeer(a, runnerwire.PeerConfig{})
	t.Cleanup(func() { p.Close(); _ = b.Close() })
	return p
}

func refCall(t *testing.T, ref string, gen int) json.RawMessage {
	t.Helper()
	data, err := runnerwire.Marshal(runnerwire.RefArgs{Ref: ref, PolicyGeneration: gen})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A stop, kill or end the org sends through the dispatcher, of a macOS VM whose helper outlives its kill, is
// answered as a teardown the runner could not see finish: never persisted as done (AfterRefCall), finished
// once with that cause, and with no disk report. Once the helper has exited, the same call is done,
// persisted once, and answered with the disk's removal.
func TestDispatcherMacVMTeardownWithTheHelperAliveIsNotDone(t *testing.T) {
	for _, method := range []string{runnerwire.MethodStop, runnerwire.MethodKill, runnerwire.MethodEnd} {
		t.Run(method, func(t *testing.T) {
			r := newMacRig(t)
			r.c.helperWait = 100 * time.Millisecond
			sb, spec := createMac(t, r)
			d, prefix, c := composedDispatcher(t, r.d)
			peer := onlinePeer(t)
			r.host.mu.Lock()
			r.host.pinned = map[int]bool{macHelperPID: true}
			r.host.mu.Unlock()

			res, err := d.Call(t.Context(), peer, 1, method, refCall(t, prefix+sb.Ref, 0))
			unconfirmed := (*runner.TeardownUnconfirmedError)(nil)
			if !errors.Is(err, errMacVMHelperAlive) || !errors.As(err, &unconfirmed) || res != nil {
				t.Fatalf("%s with the helper alive = %v, %v; want the unconfirmed teardown and no result", method, res, err)
			}
			if after, causes := c.counts(); after != 0 || len(causes) != 1 || !errors.Is(causes[0], errMacVMHelperAlive) {
				t.Fatalf("persisted %d times, finished with %v; want none persisted, finished once with the helper's", after, causes)
			}

			r.host.mu.Lock()
			r.host.pinned = nil
			r.host.mu.Unlock()
			r.host.gone(macHelperPID)
			res, err = d.Call(t.Context(), peer, 2, method, refCall(t, prefix+sb.Ref, 0))
			if err != nil {
				t.Fatalf("%s once the helper exited = %v", method, err)
			}
			reply, ok := res.(runnerwire.TeardownResult)
			if !ok || reply.MacOSVMDisk == nil || reply.MacOSVMDisk.State != runner.MacOSVMDiskRemoved || reply.MacOSVMDisk.RunID != spec.RunID {
				t.Fatalf("reply = %#v, want the run's disk removed", res)
			}
			if after, causes := c.counts(); after != 1 || len(causes) != 2 || causes[1] != nil {
				t.Fatalf("persisted %d times, finished with %v; want one persisted, the second finished clean", after, causes)
			}
		})
	}
}

// failWipe makes every AWS cache wipe fail, as a wipe that cannot reach the kept layer does.
func failWipe(t *testing.T) {
	old := awsCacheWipeArgv
	awsCacheWipeArgv = []string{"sh", "-c", "exit 3"}
	t.Cleanup(func() { awsCacheWipeArgv = old })
}

// wipeSpec is a Linux VM run with an AWS profile record and two shares.
func wipeSpec(t *testing.T) runner.SandboxSpec {
	spec := vmSpec()
	spec.RunID = uuid.New()
	spec.Env = map[string]string{runner.AWSProfileRecordEnv: "cHJvZmlsZQ=="}
	spec.Mounts = []runner.Mount{{Source: t.TempDir(), Target: "/home/agent/work"}, {Source: t.TempDir(), Target: "/workspace/ref", ReadOnly: true}}
	return spec
}

// failShareOnce arms share 1 of run to fail its first removal: the end answers its wipe warning beside a
// share left, then the warning alone. It answers the share's name.
func failShareOnce(r *vmRig, run uuid.UUID) string {
	second := vmfsName(run, 1)
	r.f.mu.Lock()
	r.f.failRemove = map[string]int{second: 1}
	r.f.mu.Unlock()
	return second
}

// wipeVM starts a wipeSpec run on a new rig and arms failShareOnce.
func wipeVM(t *testing.T) (*vmRig, runner.Sandbox, string) {
	t.Helper()
	r := newVMRig(t)
	spec := wipeSpec(t)
	sb, err := r.d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	return r, sb, failShareOnce(r, spec.RunID)
}

// The dispatcher's end, with no policy generation (EndSandbox) and with one (EndSandboxAt): an end whose
// AWS cache wipe failed beside a share that would not go is not done and not persisted; the retry, whose
// only warning is the wipe, is done and persisted once.
func TestDispatcherEndTakesAWipeWarningAloneAsDone(t *testing.T) {
	failWipe(t)
	for _, gen := range []int{0, 1} {
		t.Run(map[int]string{0: "no generation", 1: "generation 1"}[gen], func(t *testing.T) {
			r, sb, second := wipeVM(t)
			d, prefix, c := composedDispatcher(t, r.d)
			peer := onlinePeer(t)
			if _, err := d.Call(t.Context(), peer, 1, runnerwire.MethodEnd, refCall(t, prefix+sb.Ref, gen)); err == nil || !strings.Contains(err.Error(), second) || errors.Is(err, runner.ErrAWSCacheWipeFailed) {
				t.Fatalf("end with a share left = %v, want the share's failure and no wipe warning", err)
			}
			if after, _ := c.counts(); after != 0 {
				t.Fatalf("an end that left a share was persisted as done %d times", after)
			}
			if _, err := d.Call(t.Context(), peer, 2, runnerwire.MethodEnd, refCall(t, prefix+sb.Ref, gen)); err != nil {
				t.Fatalf("the retry = %v, want the wipe warning taken as a finished end", err)
			}
			if after, causes := c.counts(); after != 1 || len(causes) != 2 || causes[1] != nil {
				t.Fatalf("persisted %d times, finished with %v; want the retry persisted once and finished clean", after, causes)
			}
			if !r.container(t, second).removed {
				t.Fatal("the retry left share 1")
			}
		})
	}
}

// The org's reading of an end (runner.SplitAWSCacheWipe, which the lease end and the generation-fenced
// stop of a lost run apply): a share left beside the wipe warning is a failed end with no wipe words, and
// the retry's warning alone is a finished end whose words are kept for the run.ended row.
func TestOrgReadsADriverEndsWipeWarning(t *testing.T) {
	failWipe(t)
	for name, end := range map[string]func(*Driver, string) error{
		"EndSandbox":   func(d *Driver, ref string) error { return d.EndSandbox(context.Background(), ref) },
		"EndSandboxAt": func(d *Driver, ref string) error { return d.EndSandboxAt(context.Background(), ref, 1) },
	} {
		t.Run(name, func(t *testing.T) {
			r, sb, second := wipeVM(t)
			if words, err := runner.SplitAWSCacheWipe(end(r.d, sb.Ref)); words != "" || err == nil || !strings.Contains(err.Error(), second) {
				t.Fatalf("with a share left = %q, %v; want no wipe words and the share's failure", words, err)
			}
			if words, err := runner.SplitAWSCacheWipe(end(r.d, sb.Ref)); err != nil || !strings.Contains(words, runner.ErrAWSCacheWipeFailed.Error()) {
				t.Fatalf("the retry = %q, %v; want a finished end with the wipe's words", words, err)
			}
		})
	}
}

// A queued end the offline manager applies on a fresh session, over the real Driver: an end that left a
// share sends no receipt and fails Pending, and the next session's Pending, whose end answers the wipe
// warning alone, sends the end's success receipt.
func TestPendingEndTakesAWipeWarningAloneAsDone(t *testing.T) {
	failWipe(t)
	r := newVMRig(t)
	runnerID := uuid.New()
	m, err := offline.New(t.TempDir(), runnerID, r.d, SandboxRef,
		func(context.Context, uuid.UUID, string) (int64, bool, error) { return 0, true, nil }, func(uuid.UUID) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	ctx := t.Context()
	org := time.Now().UTC()
	if err := m.Clock(runnerwire.Hello{ServerTime: org}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.BeforeReady(ctx, &runnerwire.Peer{}, false); err != nil {
		t.Fatal(err)
	}
	spec := wipeSpec(t)
	if err := m.Lease(ctx, runnerwire.Lease{RunID: spec.RunID, TokenRenewedAt: org, EndsAt: org.Add(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := m.BeforeCreate(ctx, spec); err != nil {
		t.Fatal(err)
	}
	sb, err := r.d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Created(ctx, spec, sb.Ref); err != nil {
		t.Fatal(err)
	}
	if err := m.MutationFinished(ctx, runnerwire.MethodCreateSandbox, "", spec.RunID, uuid.Nil, nil); err != nil {
		t.Fatal(err)
	}
	second := failShareOnce(r, spec.RunID)
	m.Offline(ctx)

	peer, receipts := composedReceiptPeer(ctx, t)
	end := runnerwire.PendingAction{ID: uuid.New(), RunID: spec.RunID, Ref: placement.RunnerRefPrefix(runnerID.String()) + sb.Ref, Kind: types.RunnerActionEnd}
	if err := m.Pending(ctx, peer, []runnerwire.PendingAction{end}); err == nil || !strings.Contains(err.Error(), second) {
		t.Fatalf("Pending with a share left = %v, want the share's failure", err)
	}
	select {
	case got := <-receipts:
		t.Fatalf("an end that left a share sent receipt %+v", got)
	default:
	}
	if err := m.Pending(ctx, peer, []runnerwire.PendingAction{end}); err != nil {
		t.Fatalf("the next session's Pending = %v, want the wipe warning taken as a finished end", err)
	}
	select {
	case got := <-receipts:
		if got.ID != end.ID || got.Outcome != "success" {
			t.Fatalf("receipt %+v, want the end's success", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no receipt for the finished end")
	}
	if !r.container(t, second).removed {
		t.Fatal("the retry left share 1")
	}
}

// composedReceiptPeer is the runner's end of a session whose org side hands back each action_result.
func composedReceiptPeer(ctx context.Context, t *testing.T) (*runnerwire.Peer, <-chan runnerwire.ActionResult) {
	t.Helper()
	receipts := make(chan runnerwire.ActionResult, 4)
	orgConn, runConn := runnerwire.Loopback()
	orgPeer := runnerwire.NewPeer(orgConn, runnerwire.PeerConfig{Org: true, OnEventChecked: func(event runnerwire.Event) error {
		var result runnerwire.ActionResult
		if err := json.Unmarshal(event.Data, &result); err != nil {
			return err
		}
		receipts <- result
		return nil
	}})
	peer := runnerwire.NewPeer(runConn, runnerwire.PeerConfig{})
	orgPeer.Start(ctx, 0)
	peer.Start(ctx, 0)
	t.Cleanup(func() { orgPeer.Close(); peer.Close() })
	return peer, receipts
}
