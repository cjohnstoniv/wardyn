// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// on-demand launcher

// uiEnsureApp makes sure the declared port is actually listening inside the
// sandbox, starting the app if it is not. ONE exec does the whole cycle —
// probe, launch, poll — because every extra round trip here is a full exec
// into the sandbox; its exit code is the answer:
//
//	0   already listening — nothing was started
//	5   the launcher ran and the port came up (this is what ui.start records)
//	3   the image has no launcher at the convention path (the BYOI case)
//	4   the launcher ran but nothing opened the port in time
//
// A successful probe is cached briefly (uiReadyTTL) so a browser opening six
// connections at once does not re-probe six times.
//
// ponytail: two simultaneous cold connections can both launch, since each
// probes before either launches. Harmless — the port is exclusive, so exactly
// one process keeps it and the loser exits; a per-app lock would buy nothing
// but contention.
func (s *Server) uiEnsureApp(ctx context.Context, run types.AgentRun, sess uiSession) error {
	key := sess.Run.String() + "/" + sess.App
	if s.uiAppReady(key) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, uiEnsureTimeout)
	defer cancel()

	launcher := uiLauncherPrefix + sess.App
	// A run with a boot seed (an application's startup command, #1994) brings
	// its own app up: with no launcher, wait for the port before calling it
	// missing, so an app still starting is not reported as one never shipped.
	seedPolls := 0
	if interactiveBootSeed(run.Interactive, run.Task) != "" {
		seedPolls = uiEnsureWaitSecs * 2
	}
	exec, err := s.cfg.Runner.ExecStream(ctx, run.SandboxRef, runner.ExecSpec{
		Argv: []string{"sh", "-c", uiLauncherScript(launcher, sess.Port, seedPolls)},
	})
	if err != nil {
		return uiFail(ctx, http.StatusBadGateway, reasonUIGatewayLauncherExecFailed, uiExecFailMsg(ctx, "start ui app launcher", err))
	}
	// Both streams MUST be drained before Wait: they are unbuffered pipes off
	// one demux goroutine (runner.ExecSession's contract), so an undrained byte
	// deadlocks the wait below.
	if exec.Stdout != nil {
		go func() { _, _ = io.Copy(io.Discard, exec.Stdout) }()
	}
	if exec.Stderr != nil {
		go func() { _, _ = io.Copy(io.Discard, exec.Stderr) }()
	}
	code := -1
	if exec.Wait != nil {
		code, err = exec.Wait()
		if err != nil {
			code = -1
		}
	}
	if exec.Close != nil {
		_ = exec.Close()
	}

	switch code {
	case 0: // already listening; nothing was started, so nothing to record
		s.markUIAppReady(key)
		return nil
	case 5:
		s.markUIAppReady(key)
		s.auditUISession(sess, "ui.start", sess.App, "success",
			map[string]any{"app": sess.App, "port": sess.Port, "launcher": launcher})
		return nil
	case 3:
		// FROZEN string: the console prints this verbatim under its
		// missing-launcher error (docs/design/ui-sandboxes-prompt.md §7).
		return uiFail(ctx, http.StatusBadGateway, reasonUIGatewayLauncherMissing,
			"no UI launcher in this image: "+launcher+" not found")
	case 4:
		s.auditUISession(sess, "ui.start", sess.App, "failure",
			map[string]any{"app": sess.App, "port": sess.Port, "reason": "did not listen in time"})
		return uiFail(ctx, http.StatusBadGateway, reasonUIGatewayLauncherNotListening,
			fmt.Sprintf("%s started but nothing was listening on 127.0.0.1:%d after %ds", launcher, sess.Port, uiEnsureWaitSecs))
	default:
		return uiFail(ctx, http.StatusBadGateway, reasonUIGatewayLauncherProbeFailed,
			fmt.Sprintf("could not start UI app %q inside the sandbox (probe exit %d; the image needs sh and socat)", sess.App, code))
	}
}

// uiLauncherScript is the in-sandbox probe-launch-poll cycle. Both
// interpolations are policy-validated (a lower-case slug and an integer port,
// validateUIApps), so neither can carry a shell metacharacter; they are quoted
// anyway.
//
// ponytail: no setsid/nohup. The exec has no TTY, so there is no SIGHUP to
// survive; the launched process is reparented to the sandbox's PID 1 and dies
// with the sandbox — which is exactly the lifetime a relayed app should have.
//
// seedPolls is how many half-second polls a missing launcher waits for the
// port before exit 3: 0 answers at once; a seeded run's startup command may
// still be opening it, and if it does the answer is exit 0, nothing started.
func uiLauncherScript(launcher string, port, seedPolls int) string {
	return fmt.Sprintf(`p=%d
b='%s'
socat -u /dev/null "TCP:127.0.0.1:$p" >/dev/null 2>&1 && exit 0
if ! [ -x "$b" ]; then
  i=0
  while [ "$i" -lt %d ]; do
    sleep 0.5
    socat -u /dev/null "TCP:127.0.0.1:$p" >/dev/null 2>&1 && exit 0
    i=$((i+1))
  done
  exit 3
fi
"$b" >/dev/null 2>&1 </dev/null &
i=0
while [ "$i" -lt %d ]; do
  sleep 0.5
  socat -u /dev/null "TCP:127.0.0.1:$p" >/dev/null 2>&1 && exit 5
  i=$((i+1))
done
exit 4
`, port, launcher, seedPolls, uiEnsureWaitSecs*2)
}

func (s *Server) uiAppReady(key string) bool {
	s.uiReadyMu.Lock()
	defer s.uiReadyMu.Unlock()
	at, ok := s.uiReady[key]
	return ok && s.cfg.Now().Sub(at) < uiReadyTTL
}

func (s *Server) markUIAppReady(key string) {
	s.uiReadyMu.Lock()
	defer s.uiReadyMu.Unlock()
	if s.uiReady == nil {
		s.uiReady = map[string]time.Time{}
	} else if len(s.uiReady) > 1024 {
		// Pruned wholesale past a bound, like shouldTouch's map: the worst case
		// is one extra probe per live app.
		clear(s.uiReady)
	}
	s.uiReady[key] = s.cfg.Now()
}
