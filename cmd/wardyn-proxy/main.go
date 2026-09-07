// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command wardyn-proxy is the L2 per-workspace egress sidecar: an HTTP
// forward proxy that enforces Wardyn's default-deny domain allowlist, method
// rules, and first-use approval; streams decision logs to the control plane;
// and injects third-party credentials proxy-side so secrets never enter the
// sandbox. The same binary runs on the docker and k8s targets.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

// egressCanaryTimeout bounds the TCP dial the -egress-canary flag performs.
// Short: the target is always an in-cluster apiserver host:port (no DNS, no
// external egress), so a real connect either succeeds fast or the k8s
// NetworkPolicy canary wants to see the refusal/timeout promptly.
const egressCanaryTimeout = 5 * time.Second

func main() {
	configPath := flag.String("config", "", "path to wardyn-proxy JSON config (overrides WARDYN_PROXY_CONFIG_JSON)")
	// egressCanary is the k8s substrate's boot-time NetworkPolicy-enforcement
	// probe (see internal/runner/k8s): launched as a throwaway pod with this
	// flag instead of the normal proxy entrypoint, it TCP-dials host:port (the
	// apiserver, taken from the substrate's own rest.Config — never DNS, never
	// external egress) and exits 0 on connect, 1 on refuse/timeout. A Running
	// pod's exit code is what lets the substrate tell "NetworkPolicy enforced"
	// (deny-all blocks the dial) apart from "not enforced" (dial succeeds
	// anyway) — see substrate.ClassSupport.NetworkPolicy's doc. Not a normal
	// proxy invocation: no config is loaded, nothing else in main runs.
	egressCanary := flag.String("egress-canary", "", "internal: TCP-dial host:port, exit 0 on connect / 1 on refuse-or-timeout (k8s substrate canary only)")
	flag.Parse()

	if *egressCanary != "" {
		conn, err := net.DialTimeout("tcp", *egressCanary, egressCanaryTimeout)
		if err != nil {
			os.Exit(1)
		}
		_ = conn.Close()
		os.Exit(0)
	}

	var (
		cfg *proxy.Config
		err error
	)
	switch {
	case *configPath != "":
		cfg, err = proxy.LoadConfig(*configPath)
	case os.Getenv("WARDYN_PROXY_CONFIG_JSON") != "":
		// Sidecar path: the runner driver delivers the full config (incl. the
		// run's egress policy) as one env var at container create.
		cfg, err = proxy.LoadConfigBytes([]byte(os.Getenv("WARDYN_PROXY_CONFIG_JSON")))
	default:
		slog.Error("wardyn-proxy: -config or WARDYN_PROXY_CONFIG_JSON is required")
		os.Exit(1)
	}
	if err != nil {
		slog.Error("wardyn-proxy: load config failed", slog.Any("err", err))
		os.Exit(1)
	}

	// Per-proxy kill-switch: WARDYN_LLM_SCAN=off forces THIS proxy process's
	// outbound content inspection OFF regardless of policy. It can only DISABLE
	// (fail-safe direction), never enable beyond what the policy authorizes.
	// NOTE: this is a per-proxy env read only — it is not wired into either
	// deploy path (compose/Helm do not propagate it from a central config), so
	// it is NOT a fleet-wide kill-switch today; an operator would need to set
	// this env on every sidecar individually.
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv("WARDYN_LLM_SCAN"))); v {
	case "off", "0", "false", "no", "disable", "disabled", "none":
		if cfg.Policy.LLMInspection != nil {
			slog.Info("wardyn-proxy: WARDYN_LLM_SCAN kill-switch set — outbound content inspection disabled")
			cfg.Policy.LLMInspection = nil
		}
	case "", "on", "1", "true", "yes", "enable", "enabled":
		// Unset or an explicit "leave as policy" token: the switch only DISABLES,
		// so these are a no-op — inspection stays exactly as the policy authorizes.
	default:
		// A value that is neither a disable token nor an enable token states no
		// intent this kill-switch can honor. Fail loud rather than silently
		// ignore it (the operator may have typo'd "of" and think scanning is off).
		slog.Error("wardyn-proxy: WARDYN_LLM_SCAN has an unrecognized value; want off/0/false/no/disable to disable, or on/1/true/yes to leave as policy",
			slog.String("value", v))
		os.Exit(2)
	}

	// Tell the Go runtime about the sidecar's cgroup ceiling BEFORE any request
	// is served — see applyCgroupMemoryLimit.
	applyCgroupMemoryLimit()

	// State the git-broker push posture ONCE at boot — see logBranchNSPosture.
	logBranchNSPosture(cfg.RunID)

	// Startup mint of injection credentials is bounded: fail closed if the
	// broker is unreachable or an approval is still pending.
	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelStartup()

	// This client serves the control-plane API calls (injection resolve, decision
	// log, approval checks) — NOT egress forwards, which use the proxy's own
	// transport. Its timeout must EXCEED the subscription delegated-refresh budget
	// (subscription.defaultRefreshTimeout, 120s): an injection resolve at the
	// token-expiry boundary triggers that refresh server-side, and a shorter
	// client timeout would fail the resolve closed before it can complete. Decision
	// posts are async-buffered, so the longer ceiling never blocks the egress path.
	client := &http.Client{Timeout: 130 * time.Second}
	srv, err := proxy.NewServer(startupCtx, cfg, client, os.Stdout)
	if err != nil {
		slog.Error("wardyn-proxy: server startup failed", slog.Any("err", err))
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("wardyn-proxy: listening",
			slog.String("addr", srv.Addr()),
			slog.String("run_id", cfg.RunID.String()),
		)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil {
			slog.Error("wardyn-proxy: serve error", slog.Any("err", err))
			os.Exit(1)
		}
	case <-ctx.Done():
		slog.Info("wardyn-proxy: shutdown signal received")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("wardyn-proxy: shutdown error", slog.Any("err", err))
			os.Exit(1)
		}
		slog.Info("wardyn-proxy: stopped cleanly")
	}
}

// logBranchNSPosture states the git-broker push branch-namespace posture ONCE
// at boot, and only when it is OFF — the sibling of the WARDYN_LLM_SCAN line
// in main, for the one control in that path that is ON by default. WARN, not
// Info: the kill-switch above disables something the policy had to opt INTO,
// this disables a confinement every brokered run otherwise gets. A garbage
// env value already logs (fail-closed, proxy.BranchNSEnforced); before this,
// the OFF state logged nothing at all, so the only way to tell a confined
// proxy from an opted-out one was `docker inspect` on the sidecar. Per-push
// proof lives in the decision log (rule_source "brokered:git:branch-ns-off").
//
// Extracted from main() as its own seam so the one-shot boot WARN is
// testable without driving the rest of main() (a real listener, os.Exit,
// signal handling).
func logBranchNSPosture(runID uuid.UUID) {
	if !proxy.BranchNSEnforced() {
		slog.Warn("wardyn-proxy: WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS=false — push branch-namespace confinement is OFF for this proxy; a brokered git push may update ANY ref in a granted repo, including the default branch",
			slog.String("run_id", runID.String()))
	}
}

// cgroupMemoryHeadroom is the fraction of the cgroup ceiling the Go heap may
// target. The remainder covers the non-heap footprint the GC's soft limit does
// not account for at all (thread stacks, the runtime's own mappings) plus the
// slack a soft limit needs to be a BACKPRESSURE signal rather than a cliff.
const cgroupMemoryHeadroom = 0.8

// applyCgroupMemoryLimit points the Go GC's soft memory limit at the sidecar's
// own cgroup ceiling.
//
// The wardyn-proxy sidecar runs under a hard memory cap (256 MiB, with swap
// pinned equal), but nothing ever told the runtime that: with no GOMEMLIMIT the
// GC targets roughly 2x live heap and will happily grow INTO the cgroup limit
// and be OOM-killed rather than collect harder — and killing this sidecar takes
// the run's only network path with it. That matters because outbound content
// inspection buffers a request body and the extractor expands it several times
// over (F074; see maxConcurrentScans in internal/egress/proxy). A soft limit
// makes the GC work harder first, which is the whole point.
//
// Best effort and silent about a miss: an operator-set GOMEMLIMIT wins (the
// runtime has already applied it), and outside a memory-capped cgroup — the
// host-mode and unit-test cases — there is simply nothing to read.
func applyCgroupMemoryLimit() {
	if os.Getenv("GOMEMLIMIT") != "" {
		return
	}
	limit, ok := cgroupMemoryLimitBytes()
	if !ok {
		return
	}
	soft := int64(float64(limit) * cgroupMemoryHeadroom)
	if soft <= 0 {
		return
	}
	debug.SetMemoryLimit(soft)
	slog.Info("wardyn-proxy: GC soft memory limit set from the cgroup ceiling",
		slog.Int64("cgroup_bytes", limit), slog.Int64("gomemlimit_bytes", soft))
}

// cgroupMemoryLimitBytes reads this process's memory ceiling from cgroup v2
// (memory.max) or v1 (memory.limit_in_bytes). "max" — and v1's
// effectively-unlimited sentinel — report no limit.
func cgroupMemoryLimitBytes() (int64, bool) {
	for _, path := range []string{
		"/sys/fs/cgroup/memory.max",
		"/sys/fs/cgroup/memory/memory.limit_in_bytes",
	} {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		v, perr := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		// A v1 cgroup with no limit reports a sentinel near maxint; treat
		// anything at or above 1 TiB as "no meaningful cap".
		if perr != nil || v <= 0 || v >= 1<<40 {
			continue
		}
		return v, true
	}
	return 0, false
}
