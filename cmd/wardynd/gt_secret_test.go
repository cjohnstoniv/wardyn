// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeSecret records every patch the Secret writer sends, as the API server
// would merge it into the named Secret's data.
type fakeSecret struct {
	mu      sync.Mutex
	patches int
	data    map[string][]byte
	err     error
}

func (f *fakeSecret) patch(_ context.Context, data map[string][]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.patches++
	if f.err != nil {
		return f.err
	}
	if f.data == nil {
		f.data = map[string][]byte{}
	}
	for k, v := range data {
		f.data[k] = v
	}
	return nil
}

func (f *fakeSecret) snapshot() (int, map[string][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]byte{}
	for k, v := range f.data {
		out[k] = v
	}
	return f.patches, out
}

// stubRotateAfter swaps the rotator's wait for a channel the test fires, and
// records each wait the rotator asked for.
func stubRotateAfter(t *testing.T) (waits chan time.Duration, fire chan time.Time) {
	t.Helper()
	waits = make(chan time.Duration, 16)
	fire = make(chan time.Time)
	prev := gtRotateAfter
	gtRotateAfter = func(d time.Duration) <-chan time.Time {
		waits <- d
		return fire
	}
	t.Cleanup(func() { gtRotateAfter = prev })
	return waits, fire
}

func nextWait(t *testing.T, waits chan time.Duration) time.Duration {
	t.Helper()
	select {
	case d := <-waits:
		return d
	case <-time.After(2 * time.Second):
		t.Fatal("the rotator never scheduled its next mint")
		return 0
	}
}

// The Secret writer puts the token and the internal CA into the named Secret
// on the first mint, then patches again before half the token's lifetime is
// gone, so the sensor's mounted copy never expires.
func TestRotator_SecretPatchesTokenAndCAThenRepatchesBeforeHalfTTL(t *testing.T) {
	waits, fire := stubRotateAfter(t)
	sec := &fakeSecret{}
	m := &fakeGTMinter{token: "tok-1", expiry: time.Now().Add(time.Hour)}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runGroundtruthTokenRotator(ctx, m, secretTokenWriter(sec.patch, "-----BEGIN CERTIFICATE-----\nCA\n"))
		close(done)
	}()

	if d := nextWait(t, waits); d > 30*time.Minute || d < time.Minute {
		t.Fatalf("after a 1h token the next patch must come within half its lifetime, got %v", d)
	}
	n, data := sec.snapshot()
	if n != 1 || string(data["token"]) != "tok-1" || string(data["control-plane-ca.pem"]) != "-----BEGIN CERTIFICATE-----\nCA\n" {
		t.Fatalf("first mint must patch token and CA once; patches=%d data=%q", n, data)
	}

	fire <- time.Now()
	nextWait(t, waits)
	cancel()
	<-done
	if n, _ := sec.snapshot(); n != 2 {
		t.Fatalf("the rotator must patch the Secret again on its next tick, got %d patches", n)
	}
}

// A refused patch (RBAC, a missing Secret, an API server blip) retries in a
// minute rather than waiting the full cadence with the sensor unauthenticated.
func TestRotator_SecretPatchErrorRetriesInAMinute(t *testing.T) {
	waits, _ := stubRotateAfter(t)
	sec := &fakeSecret{err: errors.New("secrets \"wardyn-groundtruth\" is forbidden")}
	m := &fakeGTMinter{token: "tok-1", expiry: time.Now().Add(time.Hour)}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runGroundtruthTokenRotator(ctx, m, secretTokenWriter(sec.patch, "ca")); close(done) }()

	d := nextWait(t, waits)
	cancel()
	<-done
	if d != time.Minute {
		t.Fatalf("a failed patch must retry in 1m, got %v", d)
	}
}

// Without an internal TLS listener there is no CA to publish: the patch then
// carries the token alone and never blanks a CA another write put there.
func TestSecretTokenWriter_NoCAPatchesTokenOnly(t *testing.T) {
	sec := &fakeSecret{data: map[string][]byte{"control-plane-ca.pem": []byte("old")}}
	if err := secretTokenWriter(sec.patch, "")(context.Background(), "tok"); err != nil {
		t.Fatal(err)
	}
	_, data := sec.snapshot()
	if string(data["token"]) != "tok" || string(data["control-plane-ca.pem"]) != "old" {
		t.Fatalf("data = %q", data)
	}
}

func TestParseGroundtruthTokenSecret(t *testing.T) {
	ns, name, err := parseGroundtruthTokenSecret("kube-system/wardyn-groundtruth")
	if err != nil || ns != "kube-system" || name != "wardyn-groundtruth" {
		t.Fatalf("got %q %q %v", ns, name, err)
	}
	for _, bad := range []string{"wardyn-groundtruth", "/name", "ns/", "a/b/c", "NS/name", "ns/Name_1", "ns/ name"} {
		if _, _, err := parseGroundtruthTokenSecret(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

// One delivery only: the file and the Secret are two answers to the same
// question, so setting both refuses boot instead of picking one silently.
func TestGroundtruthTokenDelivery_FileAndSecretRefused(t *testing.T) {
	t.Setenv("WARDYN_GROUNDTRUTH_TOKEN_FILE", t.TempDir()+"/gt-token")
	t.Setenv("WARDYN_GROUNDTRUTH_TOKEN_SECRET", "ns/name")
	if _, _, err := groundtruthTokenDelivery(nil); err == nil {
		t.Fatal("both set must refuse boot")
	}
}

func TestGroundtruthTokenDelivery_BadSecretRefused(t *testing.T) {
	t.Setenv("WARDYN_GROUNDTRUTH_TOKEN_FILE", "")
	t.Setenv("WARDYN_GROUNDTRUTH_TOKEN_SECRET", "no-namespace")
	if _, _, err := groundtruthTokenDelivery(nil); err == nil {
		t.Fatal("a value that is not <namespace>/<name> must refuse boot")
	}
}

func TestGroundtruthTokenDelivery_FileModeUnchanged(t *testing.T) {
	path := t.TempDir() + "/gt-token"
	t.Setenv("WARDYN_GROUNDTRUTH_TOKEN_FILE", path)
	t.Setenv("WARDYN_GROUNDTRUTH_TOKEN_SECRET", "")
	w, k8s, err := groundtruthTokenDelivery(nil)
	if err != nil || w == nil || k8s {
		t.Fatalf("file mode: writer=%v k8s=%v err=%v", w != nil, k8s, err)
	}
	if err := w(context.Background(), "tok"); err != nil {
		t.Fatal(err)
	}
}

func TestGroundtruthTokenDelivery_UnsetIsOff(t *testing.T) {
	t.Setenv("WARDYN_GROUNDTRUTH_TOKEN_FILE", "")
	t.Setenv("WARDYN_GROUNDTRUTH_TOKEN_SECRET", "")
	w, k8s, err := groundtruthTokenDelivery(nil)
	if err != nil || w != nil || k8s {
		t.Fatalf("unset: writer=%v k8s=%v err=%v", w != nil, k8s, err)
	}
}
