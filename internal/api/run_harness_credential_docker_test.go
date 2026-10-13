// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package api

// The tool-approval credential on a real Docker daemon, from what dispatchRun
// composes to what a process in the sandbox holds. The sandbox is the real
// claude-code agent image, given this tree's agent-run, so the launch under test
// is the one that ships; its claude is the image's own. Skipped unless
// WARDYN_TEST_DOCKER=1 and the image is present locally.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const credTestImage = "wardyn/agent-claude-code:local"

type credSandbox struct {
	cli  *client.Client
	ref  string
	spec runner.SandboxSpec
	cred string
}

// credentialSandbox dispatches a two-harness hold run that starts claude-code
// and creates its sandbox on the real driver. Both the sandbox and its network
// are removed by name when the test ends.
func credentialSandbox(t *testing.T) credSandbox {
	t.Helper()
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the real-Docker credential tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	if _, err := cli.ImageInspect(ctx, credTestImage); err != nil {
		t.Skipf("%s is not built locally: %v", credTestImage, err)
	}
	rn := &fakeRunner{}
	dispatchTwoHarnessRun(t, rn, dispatchParams{ToolApprovals: "hold"})
	spec := rn.lastSpec
	cred := toolgateCredentialOf(t, spec)
	spec.Image = credTestImage
	spec.ProxyConfig.ControlPlaneURL = "http://127.0.0.1:0"

	network := "wardyn-credtest-" + uuid.NewString()[:8]
	if _, err := cli.NetworkCreate(ctx, network, client.NetworkCreateOptions{Driver: "bridge"}); err != nil {
		t.Fatalf("network create: %v", err)
	}
	t.Cleanup(func() { _, _ = cli.NetworkRemove(context.Background(), network, client.NetworkRemoveOptions{}) })
	d, err := docker.New(docker.Config{ProxyImage: "busybox:latest", ProxyCmd: []string{"sleep", "infinity"}, InternalNetwork: network})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	sb, err := d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	t.Cleanup(func() { _ = d.KillSandbox(context.Background(), sb.Ref) })
	return credSandbox{cli: cli, ref: sb.Ref, spec: spec, cred: cred}
}

// exec runs argv in the sandbox as a process docker exec starts, as the image's
// own user, and returns its stdout.
func (c credSandbox) exec(t *testing.T, argv ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	created, err := c.cli.ExecCreate(ctx, c.ref, client.ExecCreateOptions{AttachStdout: true, AttachStderr: true, Cmd: argv})
	if err != nil {
		t.Fatalf("exec create %v: %v", argv, err)
	}
	res, err := c.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		t.Fatalf("exec attach %v: %v", argv, err)
	}
	defer res.HijackedResponse.Close()
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, res.HijackedResponse.Reader); err != nil && err != io.EOF {
		t.Logf("exec %v: %v", argv, err)
	}
	if stderr.Len() > 0 {
		t.Logf("exec %v stderr: %s", argv[:min(len(argv), 3)], stderr.String())
	}
	return stdout.String()
}

// handStartedCredential is what a harness started by hand would send: the
// variable as a docker exec login shell, an interactive shell and a plain shell
// see it. Markers keep a shell's own start-up output out of the value.
func (c credSandbox) handStartedCredential(t *testing.T) string {
	t.Helper()
	const probe = `printf 'CRED<%s>' "${WARDYN_TOOLGATE_CREDENTIAL-}"`
	got := ""
	for _, argv := range [][]string{{"bash", "-lc", probe}, {"bash", "-ic", probe}, {"sh", "-c", probe}} {
		out := c.exec(t, argv...)
		_, v, ok := strings.Cut(out, "CRED<")
		v, _, closed := strings.Cut(v, ">")
		if !ok || !closed {
			t.Fatalf("%v printed no probe: %q", argv[:2], out)
		}
		if v != "" {
			t.Errorf("%v sees a credential", argv[:2])
			got = v
		}
	}
	return got
}

// The credential is in neither the container's environment nor a docker exec
// process's, and it is in the agent-owned 0400 file under its reserved name.
func TestToolgateCredential_NotInContainerEnv(t *testing.T) {
	c := credentialSandbox(t)
	insp, err := c.cli.ContainerInspect(context.Background(), c.ref, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	for _, kv := range insp.Container.Config.Env {
		if strings.HasPrefix(kv, "WARDYN_TOOLGATE_CREDENTIAL=") || strings.Contains(kv, c.cred) {
			t.Errorf("the container's env carries the credential: %s", strings.SplitN(kv, "=", 2)[0])
		}
	}
	if strings.Contains(c.exec(t, "sh", "-c", "env"), c.cred) {
		t.Error("a docker exec process's env carries the credential")
	}
	file := runner.ComponentSecretDir + "/" + toolgateCredentialFilePrefix + "claude-code"
	if got := c.exec(t, "sh", "-c", "stat -c '%u %a' "+file+"; cat "+file); got != "1000 400\n"+c.cred {
		t.Errorf("the credential file as the agent sees it = %q, want the agent's 0400 file holding the credential", got)
	}
}

// The started harness's own process has the credential, and so does its MCP
// child (the gate claude starts from --mcp-config), but neither a process the
// launcher starts beside it nor one started by docker exec does. claude here is
// the image's own: the shim on PATH records its environment and execs it, and
// the gate it starts is a stand-in that records its own and ends claude.
func TestToolgateCredential_StartedHarnessOnly(t *testing.T) {
	c := credentialSandbox(t)
	run, err := os.ReadFile("../../deploy/images/claude-code/agent-run")
	if err != nil {
		t.Fatal(err)
	}
	lib, err := os.ReadFile("../../deploy/images/common/agent-run-lib.sh")
	if err != nil {
		t.Fatal(err)
	}
	run = bytes.Replace(run, []byte("source /usr/local/bin/agent-run-lib.sh"), []byte("source $HOME/t/agent-run-lib.sh"), 1)
	const shims = `set -e
real=$(command -v claude)
mkdir -p ~/t ~/fakebin
printf %s "$1" | base64 -d > ~/t/agent-run
printf %s "$2" | base64 -d > ~/t/agent-run-lib.sh
printf '#!/bin/sh\nprintf %%s "${WARDYN_TOOLGATE_CREDENTIAL-}" > "$HOME/harness.cred"\nexec %s "$@"\n' "$real" > ~/fakebin/claude
printf '#!/bin/sh\nprintf %%s "${WARDYN_TOOLGATE_CREDENTIAL-}" > "$HOME/mcp-child.cred"\nkill "$PPID"\n' > ~/fakebin/wardyn-toolgate
printf '#!/bin/sh\nprintf %%s "${WARDYN_TOOLGATE_CREDENTIAL-}" >> "$HOME/sibling.cred"\nexec /bin/mkdir "$@"\n' > ~/fakebin/mkdir
chmod +x ~/fakebin/*
: > ~/sibling.cred`
	c.exec(t, "bash", "-c", shims, "shims", base64.StdEncoding.EncodeToString(run), base64.StdEncoding.EncodeToString(lib))
	out := c.exec(t, "bash", "-c", `PATH=$HOME/fakebin:$PATH timeout 90 bash ~/t/agent-run "say hi" >/dev/null 2>&1; echo "rc=$?"`)
	t.Logf("agent-run: %s", strings.TrimSpace(out))
	for file, want := range map[string]string{"harness.cred": c.cred, "mcp-child.cred": c.cred, "sibling.cred": ""} {
		if got := c.exec(t, "sh", "-c", "cat ~/"+file+" 2>/dev/null || echo ABSENT"); got != want {
			t.Errorf("%s holds the credential: %v, want %v (absent: %v)", file, got == c.cred || strings.Contains(got, c.cred), want != "", got == "ABSENT\n")
		}
	}
	if got := c.handStartedCredential(t); got != "" {
		t.Error("a docker exec process holds the credential")
	}
}

// A harness started by hand sends no credential, so the real proxy, booted from
// the config dispatch wrote, holds its call for a human under the strictest of
// the sets: claude-code allows Read and codex-cli holds it. The same call with
// the started harness's credential is claude-code's, and is allowed.
func TestToolgateCredential_HandStartedTakesMissingCredentialHold(t *testing.T) {
	c := credentialSandbox(t)
	var raised atomic.Int32
	cp := httptestApprovalsServer(t, &raised)
	port := freeLoopbackPort(t)
	pc := c.spec.ProxyConfig
	pc.ControlPlaneURL = cp
	raw, err := runner.BuildProxyConfig(c.spec.RunID, pc, port)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := proxy.LoadConfigBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen = "127.0.0.1:" + strconv.Itoa(port)
	psrv, err := proxy.NewServer(context.Background(), cfg, &http.Client{Timeout: 5 * time.Second}, io.Discard)
	if err != nil {
		t.Fatalf("proxy: %v", err)
	}
	go func() { _ = psrv.ListenAndServe() }()
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = psrv.Shutdown(sctx)
	})
	// What wardyn-toolgate sends: a Bearer only when it was given a credential.
	raise := func(cred string) (int, string) {
		t.Helper()
		var resp *http.Response
		for range 50 {
			req, _ := http.NewRequest(http.MethodPost, "http://"+cfg.Listen+"/wardyn/v1/approvals",
				strings.NewReader(`{"kind":"tool_call","payload":{"tool":"Read","cmd":"x"}}`))
			if cred != "" {
				req.Header.Set("Authorization", "Bearer "+cred)
			}
			if resp, err = http.DefaultClient.Do(req); err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("proxy unreachable: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	hand := c.handStartedCredential(t)
	if code, body := raise(hand); code != http.StatusCreated || raised.Load() != 1 {
		t.Errorf("hand-started Read: %d %s (raised %d), want a held approval", code, body, raised.Load())
	}
	if code, body := raise(c.cred); code != http.StatusOK || !strings.Contains(body, "APPROVED") || raised.Load() != 1 {
		t.Errorf("started harness Read: %d %s (raised %d), want claude-code's allow with no raise", code, body, raised.Load())
	}
}

// httptestApprovalsServer is the control plane the proxy raises a held call to:
// it counts each approval created and answers it pending.
func httptestApprovalsServer(t *testing.T, raised *atomic.Int32) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "approvals") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raised.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: uuid.New(), RunID: uuid.New(), Kind: types.ApprovalToolCall, State: types.ApprovalPending})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}
