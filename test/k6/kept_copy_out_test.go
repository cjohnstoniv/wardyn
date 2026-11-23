// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k6_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/k8s"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// caseKeptCopyOut is what the copy-out door into a kept run stands on: a confirmed StopProxy deletes the proxy pod
// and leaves the agent pod running, an exec into it works, and the agent has no egress.
func caseKeptCopyOut(t *testing.T, e *env) {
	d := e.main
	id := uuid.New()
	e.cleanupRun(t, id)
	e.setSched(t, e.pin)
	// The plain harness's spec carries no egress policy, and a proxy without one denies everything
	// (runner.ProxyConfig.Policy), so the control's one host is allowed here.
	sp := e.spec(id)
	sp.ProxyConfig.Policy = types.RunPolicySpec{AllowedDomains: []string{"example.com"}}
	if _, err := d.CreateSandbox(bg, sp); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	// The control: the agent's egress works before the stop, so its refusal after it is the stop's doing.
	for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(3 * time.Second) {
		out, _ := keptExec(t, d, id, "timeout", "30", "agentprobe", "connect", "example.com:443")
		if strings.Contains(out, "status 200") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("control example.com:443 = %q, want status 200 before the stop (proxy not serving)", out)
		}
	}
	if err := d.StopProxy(bg, ref(id)); err != nil {
		t.Fatalf("StopProxy on a healthy node: %v", err)
	}
	if p := e.evidence(t, id).Pods[proxyComponent]; p == nil || p.Witness == nil || p.Witness.Arm != "terminal" {
		t.Fatalf("proxy evidence after the stop: %+v", p)
	}
	agent, err := e.admin.CoreV1().Pods(e.runsNS).Get(bg, ref(id), metav1.GetOptions{})
	if err != nil || agent.DeletionTimestamp != nil || agent.Status.Phase != corev1.PodRunning {
		t.Fatalf("agent pod after a confirmed proxy stop: %v, want Running and not deleting", podOrErr(agent, err))
	}
	if out, code := keptExec(t, d, id, "cat", "/etc/hostname"); code != 0 || strings.TrimSpace(out) == "" {
		t.Fatalf("exec into the kept agent: exit %d, output %q", code, out)
	}
	if out, _ := keptExec(t, d, id, "timeout", "30", "agentprobe", "connect", "example.com:443"); strings.Contains(out, "status 200") {
		t.Fatalf("the kept agent reached example.com:443 with its proxy stopped: %q", out)
	}
	note(t, "agent pod Running after a confirmed proxy stop; exec works; no egress")
	if err := d.StopSandbox(bg, ref(id)); err != nil {
		t.Fatalf("StopSandbox: %v", err)
	}
	e.waitRunGone(t, id, goneWait)
}

// keptExec runs argv in run id's agent pod and returns its stdout then its stderr, each copied into its own buffer
// (runner.ExecSession's streaming contract), and its exit code.
func keptExec(t *testing.T, d *k8s.Driver, id uuid.UUID, argv ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(bg, 60*time.Second)
	defer cancel()
	sess, err := d.ExecStream(ctx, ref(id), runner.ExecSpec{Argv: argv})
	if err != nil {
		t.Fatalf("exec %v: %v", argv, err)
	}
	defer func() { _ = sess.Close() }()
	var stdout, stderr bytes.Buffer
	errDone := make(chan struct{})
	go func() {
		defer close(errDone)
		_, _ = io.Copy(&stderr, sess.Stderr)
	}()
	_, _ = io.Copy(&stdout, sess.Stdout)
	<-errDone
	code, err := sess.Wait()
	if err != nil {
		t.Fatalf("exec %v: wait: %v", argv, err)
	}
	return stdout.String() + stderr.String(), code
}

// podOrErr is a pod's phase and deletion mark for a failure message, or the error that read it.
func podOrErr(p *corev1.Pod, err error) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("phase %s, deletion %v", p.Status.Phase, p.DeletionTimestamp)
}
