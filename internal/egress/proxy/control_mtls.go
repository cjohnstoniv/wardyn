// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/svidsource"
)

// runCertificateOpenTimeout bounds the wait for the SPIRE agent's first SVID and bundle at proxy start.
const runCertificateOpenTimeout = 60 * time.Second

// withRunCertificate makes cp, the control-plane client config, present the run's X.509-SVID when the
// control plane asks for a client certificate. An empty id (the default) returns nil and leaves cp exactly
// as it was. Otherwise it opens the SPIRE source for id through open, and every handshake takes the
// source's current SVID, so a rotation reaches the next connection. cp's root pool, the internal CA pin, is
// never touched. The returned Closer is the source's; the caller closes it when the proxy stops. An error
// names why the source did not open and never carries certificate bytes.
func withRunCertificate(ctx context.Context, cp *tls.Config, id string, open func(context.Context, string, spiffeid.ID) (svidsource.Source, error)) (io.Closer, error) {
	if id == "" {
		return nil, nil
	}
	want, err := spiffeid.FromString(id)
	if err != nil {
		return nil, fmt.Errorf("control-plane client certificate unavailable: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, runCertificateOpenTimeout)
	defer cancel()
	src, err := open(ctx, identity.ProxyWorkloadAPIAddr, want)
	if err != nil {
		return nil, fmt.Errorf("control-plane client certificate unavailable: %w", err)
	}
	cp.GetClientCertificate = tlsconfig.GetClientCertificate(src)
	return src, nil
}

// closeUnlessStarted closes c, the run certificate's source, when the proxy start that opened it failed, so a
// failed start never leaves the source watching the agent's socket.
func closeUnlessStarted(c io.Closer, started *bool) {
	if !*started && c != nil {
		_ = c.Close()
	}
}
