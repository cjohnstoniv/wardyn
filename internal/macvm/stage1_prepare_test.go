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
}

func (n *relayNet) DialGuest(ctx context.Context, port int) (net.Conn, error) {
	n.once.Do(func() { n.srv.SetPassword(n.fb.Last().Password) })
	return n.srv.Dial(ctx, port)
}

func stage1Prep(t *testing.T) (*prepFixture, *stage1test.Server) {
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
	return f, srv
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
	steps := len(srv.Execs())
	for n := 1; n <= steps+1; n++ {
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
