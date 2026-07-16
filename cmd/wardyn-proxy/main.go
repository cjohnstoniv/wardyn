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
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

func main() {
	configPath := flag.String("config", "", "path to wardyn-proxy JSON config (overrides WARDYN_PROXY_CONFIG_JSON)")
	flag.Parse()

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
		log.Fatal("wardyn-proxy: -config or WARDYN_PROXY_CONFIG_JSON is required")
	}
	if err != nil {
		log.Fatalf("wardyn-proxy: %v", err)
	}

	// Per-proxy kill-switch: WARDYN_LLM_SCAN=off forces THIS proxy process's
	// outbound content inspection OFF regardless of policy. It can only DISABLE
	// (fail-safe direction), never enable beyond what the policy authorizes.
	// NOTE: this is a per-proxy env read only — it is not wired into either
	// deploy path (compose/Helm do not propagate it from a central config), so
	// it is NOT a fleet-wide kill-switch today; an operator would need to set
	// this env on every sidecar individually.
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WARDYN_LLM_SCAN"))) {
	case "off", "0", "false", "no", "disable", "disabled", "none":
		if cfg.Policy.LLMInspection != nil {
			log.Printf("wardyn-proxy: WARDYN_LLM_SCAN kill-switch set — outbound content inspection disabled")
			cfg.Policy.LLMInspection = nil
		}
	}

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
		log.Fatalf("wardyn-proxy: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("wardyn-proxy: listening on %s (run %s)", srv.Addr(), cfg.RunID)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil {
			log.Fatalf("wardyn-proxy: serve error: %v", err)
		}
	case <-ctx.Done():
		log.Printf("wardyn-proxy: shutdown signal received")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("wardyn-proxy: shutdown error: %v", err)
			os.Exit(1)
		}
		log.Printf("wardyn-proxy: stopped cleanly")
	}
}
