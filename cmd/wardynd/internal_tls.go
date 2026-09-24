// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// The proxy-facing TLS listener: wardynd's end of the hop every run's proxy
// resolves credential values over (internal/hoptls has the why).

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/hoptls"
)

// secretInternalCA is the internal CA's secret-store row: certificate and
// PKCS#8 key in one PEM blob (hoptls.NewCA).
const secretInternalCA = "wardyn-internal-ca"

// hopTLS is what boot hands the rest of the daemon: the CA certificate every
// proxy pins, and the listener's serving config. nil means the control-plane
// URL is loopback http (a local install) and there is no listener.
type hopTLS struct {
	caPEM  string
	server *tls.Config
}

func (h *hopTLS) caCertPEM() string {
	if h == nil {
		return ""
	}
	return h.caPEM
}

// loadHopTLS refuses a plaintext control-plane URL on a non-local install
// (hoptls.CheckURL — the same rule the proxy applies at start), then loads or
// mints the internal CA through the boot-key pattern (loadOrCreateSecret) and
// signs this boot's serving certificate for the URL's host. A stored CA inside
// its rotation window is replaced (hoptls.CA.Fresh).
func loadHopTLS(ctx context.Context, secrets secretKeyStore, controlURL string) (*hopTLS, error) {
	if err := hoptls.CheckURL(controlURL); err != nil {
		return nil, fmt.Errorf("refusing to start: %w", err)
	}
	u, _ := url.Parse(strings.TrimSpace(controlURL))
	if !strings.EqualFold(u.Scheme, "https") {
		slog.Warn("wardynd: WARDYN_CONTROL_PLANE_URL is loopback http — a local install; run proxies reach this daemon in plaintext on the loopback interface",
			slog.String("control_plane_url", controlURL))
		return nil, nil
	}
	now := time.Now()
	blob, err := loadOrCreateSecret(ctx, secrets, secretInternalCA,
		func(b []byte) bool {
			ca, perr := hoptls.ParseCA(b)
			return perr == nil && ca.Fresh(now)
		},
		func() ([]byte, error) {
			slog.Info("wardynd: minted and persisted the internal CA for the control-plane to proxy hop")
			return hoptls.NewCA(now)
		},
	)
	if err != nil {
		return nil, err
	}
	ca, err := hoptls.ParseCA(blob)
	if err != nil {
		return nil, fmt.Errorf("parse stored internal CA: %w", err)
	}
	cert, err := ca.ServingCert(u.Hostname(), now)
	if err != nil {
		return nil, err
	}
	return &hopTLS{
		caPEM:  string(ca.CertPEM),
		server: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13},
	}, nil
}

// internalRoutesOnly narrows the listener to what a proxy calls: the
// run-token-authenticated /api/v1/internal/ surface, plus /healthz for probes.
// The console and the rest of the API stay on -listen.
func internalRoutesOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && !strings.HasPrefix(r.URL.Path, "/api/v1/internal/") {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// startInternalListener serves the hop until Shutdown; a serve error (a bind
// failure above all) goes to errCh and ends the daemon, because every run's
// credentials depend on this listener. nil hop: no listener.
func startInternalListener(hop *hopTLS, f *bootFlags, handler http.Handler, errCh chan<- error) *http.Server {
	if hop == nil {
		return nil
	}
	addr := *f.internalListen
	srv := &http.Server{
		Addr:              addr,
		Handler:           internalRoutesOnly(handler),
		TLSConfig:         hop.server,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	go func() {
		slog.Info("wardynd: proxy-facing TLS listener (internal CA)", slog.String("listen", addr))
		if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("internal TLS listener %s: %w", addr, err)
		}
	}()
	return srv
}
