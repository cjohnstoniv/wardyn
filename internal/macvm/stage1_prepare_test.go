// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
	"github.com/cjohnstoniv/wardyn/internal/macvm/stage1"
	"github.com/cjohnstoniv/wardyn/internal/macvm/stage1/stage1test"
	"golang.org/x/crypto/ssh"
)

// relayNet is the prep relay in front of the fake guest's Remote Login. The
// guest's admin password is the one prepare gave the provisioning boot.
type relayNet struct {
	*prepNet
	srv  *stage1test.Server
	fb   *macvmtest.FirstBoot
	once sync.Once
	dhcp atomic.Uint64
	// afterOff, when set, answers every dial once Remote Login is off.
	afterOff func(ctx context.Context, port int) (net.Conn, error)
}

func (n *relayNet) DialGuest(ctx context.Context, port int) (net.Conn, error) {
	n.once.Do(func() { n.srv.SetPassword(n.fb.Last().Password) })
	if n.afterOff != nil && !n.srv.RemoteLogin() {
		return n.afterOff(ctx, port)
	}
	return n.srv.Dial(ctx, port)
}

// DHCPRequests is the fake relay's DHCP count, which tests bump.
func (n *relayNet) DHCPRequests() uint64 { return n.dhcp.Load() }

func stage1Prep(t *testing.T) (*prepFixture, *stage1test.Server) {
	f, rn := stage1PrepNet(t)
	return f, rn.srv
}

func stage1PrepNet(t *testing.T) (*prepFixture, *relayNet) {
	t.Helper()
	f := newPrep(t)
	srv := stage1test.New(t, "wardyn-admin", "")
	rn := &relayNet{prepNet: f.net, srv: srv, fb: f.fb}
	f.opts.PrepNet = func() (macvm.PrepNetwork, error) { return rn, nil }
	// The agent sits under the VM directory, so a pin written beside it
	// would be seen by the walk below.
	dir := filepath.Join(f.opts.Layout.Root, "helper")
	agent := filepath.Join(dir, "wardyn-guest-agent")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agent, []byte("fake agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.opts.Bootstrap = stage1.New(stage1.Config{AgentPath: agent, AgentVersion: "0.10.0-test", OffWait: 200 * time.Millisecond})
	return f, rn
}

// prepareText is everything prepare said: its events, its log and its error.
func prepareText(f *prepFixture, err error) string {
	var b strings.Builder
	f.ev.mu.Lock()
	for _, ev := range f.ev.evs {
		fmt.Fprintf(&b, "%+v\n", ev)
	}
	f.ev.mu.Unlock()
	b.WriteString(f.logs.String())
	if err != nil {
		b.WriteString(err.Error())
	}
	return b.String()
}

func agentUserPassword(srv *stage1test.Server) string {
	for _, e := range srv.Execs() {
		if slices.Contains(e.Argv, "-addUser") {
			return strings.Split(string(e.Stdin), "\n")[1]
		}
	}
	return ""
}

func TestPrepareStage1PublishesTheBaseOnlyAfterRemoteLoginIsOff(t *testing.T) {
	f, srv := stage1Prep(t)
	key := srv.NewHostKey(t)
	m, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if srv.RemoteLogin() || m.AgentVersion != "0.10.0-test" {
		t.Fatalf("Remote Login on %v, manifest %+v", srv.RemoteLogin(), m)
	}
	if _, err := macvm.ReadyBase(f.opts.Layout.BaseDir(0)); err != nil {
		t.Fatal(err)
	}
	text := prepareText(f, nil)
	if !strings.Contains(text, ssh.FingerprintSHA256(key)) {
		t.Fatal("the guest's host key fingerprint is not in prepare's events")
	}
	for _, secret := range []string{f.fb.Last().Password, agentUserPassword(srv)} {
		if secret == "" || strings.Contains(text, secret) {
			t.Fatalf("a password is missing or in prepare's events or log:\n%s", text)
		}
		for _, e := range srv.Execs() {
			if strings.Contains(e.Line, secret) {
				t.Fatalf("a password is in the argv %q", e.Line)
			}
		}
	}
	// The trusted host key is held in memory only: no file under the VM
	// directory holds it, so nothing outlives this prepare.
	raw := key.Marshal()
	b64 := base64.StdEncoding.EncodeToString(raw)
	_ = filepath.WalkDir(f.opts.Layout.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		if data, _ := os.ReadFile(p); bytes.Contains(data, raw) || bytes.Contains(data, []byte(b64)) {
			t.Errorf("%s holds the guest's host key", p)
		}
		return nil
	})
}

func TestPrepareStage1FailureAtEachStepPublishesNoBase(t *testing.T) {
	f, srv := stage1Prep(t)
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	execs := srv.Execs()
	steps := len(execs)
	for n := 1; n <= steps+1; n++ {
		// A probe that fails is its row's answer "not loaded", not a failed step.
		if n <= steps && slices.Contains(execs[n-1].Argv, "print") {
			continue
		}
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			f, srv := stage1Prep(t)
			if n <= steps {
				var mu sync.Mutex
				calls := 0
				srv.Fail = func([]string) bool { mu.Lock(); defer mu.Unlock(); calls++; return calls == n }
			} else {
				srv.StaysOn = true // the last check: Remote Login did not go off
			}
			_, err := macvm.Prepare(context.Background(), f.opts)
			var se *stage1.StepError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v; want a stage-1 step error", err)
			}
			if _, rerr := macvm.ReadyBase(f.opts.Layout.BaseDir(0)); rerr == nil {
				t.Fatal("a base was published although stage 1 failed")
			}
			if strings.Contains(prepareText(f, err), f.fb.Last().Password) {
				t.Fatal("the admin password is in prepare's events, log or error")
			}
		})
	}
}

// Once Remote Login is off, only a refused port 22 from a guest shown up on
// the same boot publishes the base; anything else is unknown and publishes
// nothing.
func TestPrepareStage1UnknownLivenessPublishesNoBase(t *testing.T) {
	for name, afterOff := range map[string]func(*prepFixture, *relayNet) func(context.Context, int) (net.Conn, error){
		"port 22 refused, Screen Sharing silent": func(*prepFixture, *relayNet) func(context.Context, int) (net.Conn, error) {
			return func(context.Context, int) (net.Conn, error) { return nil, stage1test.Refused() }
		},
		"port 22 refused, Screen Sharing mute": func(_ *prepFixture, rn *relayNet) func(context.Context, int) (net.Conn, error) {
			return func(ctx context.Context, port int) (net.Conn, error) {
				if port == 22 {
					return nil, stage1test.Refused()
				}
				rn.srv.MuteRFB(true)
				return rn.srv.Dial(ctx, port)
			}
		},
		"port 22 times out": func(_ *prepFixture, rn *relayNet) func(context.Context, int) (net.Conn, error) {
			return func(ctx context.Context, port int) (net.Conn, error) {
				if port == 22 {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return rn.srv.Dial(ctx, port)
			}
		},
		"the VM stops during the check": func(f *prepFixture, rn *relayNet) func(context.Context, int) (net.Conn, error) {
			return func(ctx context.Context, port int) (net.Conn, error) {
				f.fb.Shutdown(nil)
				<-ctx.Done() // prepare has seen the stop
				return nil, stage1test.Refused()
			}
		},
		"the lease is asked again": func(_ *prepFixture, rn *relayNet) func(context.Context, int) (net.Conn, error) {
			return func(ctx context.Context, port int) (net.Conn, error) {
				rn.dhcp.Add(1)
				return rn.srv.Dial(ctx, port)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, rn := stage1PrepNet(t)
			rn.afterOff = afterOff(f, rn)
			_, err := macvm.Prepare(context.Background(), f.opts)
			if err == nil {
				t.Fatal("prepare succeeded")
			}
			t.Log(err)
			if _, err := macvm.ReadyBase(f.opts.Layout.BaseDir(0)); err == nil {
				t.Fatal("a base was published from a guest not shown to be up")
			}
		})
	}
}

// epochProbe is stage 1 reduced to reading prepare's GuestEpoch: once while
// the VM runs, and once after it stopped.
type epochProbe struct {
	fb                 *macvmtest.FirstBoot
	epoch              uint64
	running, afterStop error
}

func (b *epochProbe) Bootstrap(ctx context.Context, in macvm.BootstrapInput) (macvm.BootstrapResult, error) {
	b.epoch, b.running = in.GuestEpoch()
	b.fb.Shutdown(nil)
	<-ctx.Done() // prepare has seen the stop
	_, b.afterStop = in.GuestEpoch()
	return macvm.BootstrapResult{}, ctx.Err()
}

// TestPrepareGuestEpochFailsOnceTheVMStops: the epoch prepare hands stage 1
// reads the relay's DHCP count while the VM runs, and fails once it stopped.
func TestPrepareGuestEpochFailsOnceTheVMStops(t *testing.T) {
	f, rn := stage1PrepNet(t)
	rn.dhcp.Store(5)
	b := &epochProbe{fb: f.fb}
	f.opts.Bootstrap = b
	_, _ = macvm.Prepare(context.Background(), f.opts)
	if b.epoch != 5 || b.running != nil || !errors.Is(b.afterStop, macvm.ErrGuestStopped) {
		t.Fatalf("epoch while running: %d %v; after the stop: %v", b.epoch, b.running, b.afterStop)
	}
}
