// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

// sinkAdapter is the decision sink as the k8s driver drives it (k8s.DecisionSink): the per-run key,
// Register, the proxy's decision_sink value, Drain and ProducerGone (lane K9's contract).
type sinkAdapter struct {
	s   *spoolSink
	url string // tls://<relay ClusterIP>:<sink port>
}

func (a sinkAdapter) NewKey() (string, [32]byte, error) { return newSinkKey() }

func (a sinkAdapter) Register(id uuid.UUID, hash [32]byte) error { return a.s.Register(id, hash) }

func (a sinkAdapter) ProxyConfig(key string) any {
	return proxy.DecisionSinkConfig{URL: a.url, SPKISHA256: a.s.SPKIPin(), Key: key}
}

// Drain answers whatever the proxy did; its reply is the sink's own record (runnerspool.Drained).
// The proxy closes no open tunnel on drain (K9 design question 3): those end with the pod, which
// is why the caller deletes it right after and never treats a drain as evidence.
func (a sinkAdapter) Drain(ctx context.Context, id uuid.UUID) error {
	_, err := a.s.Drain(ctx, id)
	return err
}

func (a sinkAdapter) ProducerGone(ctx context.Context, id uuid.UUID) error {
	_, err := a.s.ProducerGone(ctx, id)
	return err
}

// onSinkLapsed turns a validated lapsed frame into the run's evidence that its proxy's egress
// stopped and its ProxyExited obligation. The sink refuses a second proxy instance for a run at
// hello (K9 design question 5), so a lapsed frame always speaks for the run's one proxy.
func onSinkLapsed(noteLapse func(uuid.UUID) error, exited func(uuid.UUID, string) error) func(uuid.UUID, string) {
	return func(id uuid.UUID, reason string) {
		if err := errors.Join(noteLapse(id), exited(id, "lapsed:"+reason)); err != nil {
			slog.Error("runnerd: recording a proxy's lapse", slog.String("run_id", id.String()), slog.Any("err", err))
		}
	}
}
