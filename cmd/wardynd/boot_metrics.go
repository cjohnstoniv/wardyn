// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// validateMetricsListenConfig refuses a -metrics-listen address that another
// wardynd listener already uses. The metrics listener is unauthenticated by
// design; sharing an address with the console, the proxy-facing internal
// listener, the SSH gateway or the UI-sandbox gateway would put that open
// route beside authenticated ones. Same comparison as the UI gateway's
// (sameListenAddress), so a wildcard or loopback spelling of one port is
// caught too. Empty is off: nothing to check.
func validateMetricsListenConfig(metricsListen, listen, internalListen, sshListen, uiListen string) error {
	if metricsListen == "" {
		return nil
	}
	for _, other := range []struct{ flag, addr string }{
		{"-listen", listen},
		{"-internal-listen", internalListen},
		{"-ssh-listen", sshListen},
		{"-ui-sandbox-listen", uiListen},
	} {
		if other.addr != "" && sameListenAddress(metricsListen, other.addr) {
			return fmt.Errorf("refusing to start: -metrics-listen %q is the same address as %s — "+
				"the metrics listener serves /metrics with no credential and must not share an address with an authenticated listener; "+
				"give it its own port (e.g. \":9464\") or unset WARDYN_METRICS_LISTEN to disable it", metricsListen, other.flag)
		}
	}
	return nil
}

// Bind before starting background work so a configured scraper cannot silently
// lose its listener. Plain HTTP is deliberate: the scraper sends no credential,
// and reachability is the deployment's NetworkPolicy. Empty remains off.
func startMetricsListener(rootCtx context.Context, addr string, handler http.Handler) error {
	if addr == "" {
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("metrics listener %q: %w", addr, err)
	}
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
	go goSafe("metrics.listener", func() {
		<-rootCtx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	})
	go goSafe("metrics.listener.serve", func() {
		slog.Info("wardynd: metrics listener serving GET /metrics without a credential", slog.String("listen", addr))
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("wardynd: metrics listener stopped", slog.Any("err", err))
		}
	})
	return nil
}
