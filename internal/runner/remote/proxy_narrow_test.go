// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// echoNarrow answers a narrow_proxy as a runner that took the list: its state, base matched.
func echoNarrow(a runnerwire.NarrowProxyArgs) (runnerwire.NarrowProxyResult, error) {
	return runnerwire.NarrowProxyResult{ConfigSHA256: "cfg", BaseSHA256: "base", LiveDenies: a.Deny, Changed: len(a.Deny) > 0, BaseChecked: true}, nil
}

// A runner whose caps carry no ProxyNarrow is sent nothing: the read answers ErrNarrowNotOffered, so the edit takes
// the revive path, and a narrow answers ErrNarrowNotSent. The control first: with the flag the same calls go out.
func TestNarrow_NoProxyNarrowCapSendsNoFrame(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	rig.Narrows(t, echoNarrow)
	sb, _ := create(t, rig, nil)
	ctx := ctxT(t)

	st, err := rig.Sub.NarrowProxy(ctx, sb.Ref, []string{"a.example"})
	if err != nil || !st.BaseChecked || !st.Changed || !slices.Equal(st.LiveDenies, []string{"a.example"}) {
		t.Fatalf("NarrowProxy with the cap = %+v, %v; want the runner's answer", st, err)
	}
	if _, err := rig.Sub.ProxyNarrowState(ctx, sb.Ref); err != nil {
		t.Fatalf("ProxyNarrowState with the cap = %v", err)
	}
	if got := len(rig.Server.NarrowCalls); got != 2 {
		t.Fatalf("a runner with the cap saw %d frames; want 2", got)
	}

	rig.Sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: []byte(`{"support":{"Classes":["CC1"]},"version":"0.10.0"}`)})
	if _, err := rig.Sub.ProxyNarrowState(ctx, sb.Ref); !errors.Is(err, runner.ErrNarrowNotOffered) {
		t.Errorf("ProxyNarrowState without the cap = %v; want ErrNarrowNotOffered", err)
	}
	if _, err := rig.Sub.NarrowProxy(ctx, sb.Ref, []string{"b.example"}); !errors.Is(err, runner.ErrNarrowNotSent) {
		t.Errorf("NarrowProxy without the cap = %v; want ErrNarrowNotSent", err)
	}
	if got := len(rig.Server.NarrowCalls); got != 2 {
		t.Errorf("a runner without the cap was sent %d more frames; want none", got-2)
	}
}

// Caps are held in memory only, so after wardynd restarts a runner's are unknown until it reports them again.
// A narrow then answers ErrNarrowNotSent, which the edit answers "try again", and never ErrNarrowNotOffered,
// which would send the edit down the revive path and refuse it as if the runner could not narrow.
func TestNarrow_CapsNotYetKnownIsNotSent(t *testing.T) {
	sub := remote.New(rid, &remote.Link{}, remote.Options{})
	ctx := ctxT(t)
	_, readErr := sub.ProxyNarrowState(ctx, "runner:"+rid+"/x")
	_, narrowErr := sub.NarrowProxy(ctx, "runner:"+rid+"/x", []string{"a.example"})
	for name, err := range map[string]error{"ProxyNarrowState": readErr, "NarrowProxy": narrowErr} {
		if !errors.Is(err, runner.ErrNarrowNotSent) || errors.Is(err, runner.ErrNarrowNotOffered) {
			t.Errorf("%s before any caps = %v; want ErrNarrowNotSent and not ErrNarrowNotOffered", name, err)
		}
	}
}

// An offline runner is not asked: the narrow answers ErrNarrowNotSent, nothing was sent and nothing is queued.
func TestNarrow_OfflineBeforeSendIsNotSent(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	rig.Narrows(t, echoNarrow)
	sb, _ := create(t, rig, nil)
	rig.Org.Drop()
	waitOffline(t, rig)
	_, err := rig.Sub.NarrowProxy(ctxT(t), sb.Ref, []string{"a.example"})
	if !errors.Is(err, runner.ErrNarrowNotSent) {
		t.Fatalf("NarrowProxy offline = %v; want ErrNarrowNotSent", err)
	}
	if errors.Is(err, runner.ErrNarrowUncertain) {
		t.Errorf("NarrowProxy offline = %v; nothing was sent, so it is not uncertain", err)
	}
	if len(rig.Server.NarrowCalls) != 0 || len(rig.Queued()) != 0 {
		t.Errorf("offline NarrowProxy reached the runner or the queue: frames %v, queued %v", rig.Server.NarrowCalls, rig.Queued())
	}
}

// A ref of another runner is not sent either.
func TestNarrow_ForeignRefIsNotSent(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	rig.Narrows(t, echoNarrow)
	if _, err := rig.Sub.NarrowProxy(ctxT(t), "runner:99999999-2222-3333-4444-555555555555/x", []string{"a.example"}); !errors.Is(err, runner.ErrNarrowNotSent) {
		t.Fatalf("NarrowProxy of a foreign ref = %v; want ErrNarrowNotSent", err)
	}
	if len(rig.Server.NarrowCalls) != 0 {
		t.Errorf("a foreign ref reached the runner: %v", rig.Server.NarrowCalls)
	}
}

// A link that drops after the frame was written may have applied the list: ErrNarrowUncertain, never NotSent
// (which would undo an edit the proxy may hold) and never NotListening or Unsupported (also definitive).
func TestNarrow_LinkDroppedAfterSendIsUncertain(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	rig.Narrows(t, func(a runnerwire.NarrowProxyArgs) (runnerwire.NarrowProxyResult, error) {
		rig.Org.Drop()
		return echoNarrow(a)
	})
	sb, _ := create(t, rig, nil)
	_, err := rig.Sub.NarrowProxy(ctxT(t), sb.Ref, []string{"a.example"})
	if !errors.Is(err, runner.ErrNarrowUncertain) {
		t.Fatalf("NarrowProxy across a drop = %v; want ErrNarrowUncertain", err)
	}
	for _, definitive := range []error{runner.ErrNarrowNotSent, runner.ErrNarrowNotListening, runner.ErrNarrowUnsupported, runner.ErrNarrowRefused} {
		if errors.Is(err, definitive) {
			t.Errorf("NarrowProxy across a drop = %v; it also reads as %v", err, definitive)
		}
	}
	if len(rig.Server.NarrowCalls) != 1 {
		t.Errorf("the runner saw %d frames; want the one that was sent", len(rig.Server.NarrowCalls))
	}
}

// The runner's definitive answers keep their sentinel across the wire, and BaseChecked and the list survive it.
func TestNarrow_RunnerAnswersKeepTheirSentinels(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	rig.Narrows(t, echoNarrow)
	sb, _ := create(t, rig, nil)
	ctx := ctxT(t)
	for _, want := range []error{runner.ErrNarrowUnsupported, runner.ErrNarrowRefused, runner.ErrNarrowNotListening, runner.ErrNarrowNotSent, runner.ErrNarrowUncertain} {
		rig.Server.Narrow = func(runnerwire.NarrowProxyArgs) (runnerwire.NarrowProxyResult, error) {
			return runnerwire.NarrowProxyResult{}, want
		}
		if _, err := rig.Sub.NarrowProxy(ctx, sb.Ref, []string{"a.example"}); !errors.Is(err, want) {
			t.Errorf("a runner answering %v came back as %v", want, err)
		}
	}
	// An answer outside the list (an internal error) is uncertain: the runner may have applied it.
	rig.Server.Narrow = func(runnerwire.NarrowProxyArgs) (runnerwire.NarrowProxyResult, error) {
		return runnerwire.NarrowProxyResult{}, errors.New("boom")
	}
	if _, err := rig.Sub.NarrowProxy(ctx, sb.Ref, []string{"a.example"}); !errors.Is(err, runner.ErrNarrowUncertain) {
		t.Errorf("an internal runner error came back as %v; want ErrNarrowUncertain", err)
	}
	// An empty list reads: no Deny on the wire.
	rig.Server.Narrow = echoNarrow
	if _, err := rig.Sub.ProxyNarrowState(ctx, sb.Ref); err != nil {
		t.Fatal(err)
	}
	if last := rig.Server.NarrowCalls[len(rig.Server.NarrowCalls)-1]; len(last.Deny) != 0 || last.Ref != sb.Ref {
		t.Errorf("the read sent %+v; want the ref and no deny", last)
	}
}
