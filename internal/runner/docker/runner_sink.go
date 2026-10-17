// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/moby/moby/client"
)

// DecisionSink is the part of wardyn-runnerd's decision sink the driver drives in runner mode: a key per
// proxy whose hash is kept in the run's record before the proxy exists, the proxy's decision_sink value,
// the drain before a stop, and the producer's settlement once the proxy container is gone. Every proxy
// sends its decisions there and runnerd alone writes the spool, so no proxy shares a directory with it.
type DecisionSink interface {
	NewKey() (key string, hash [32]byte, err error)
	// Register registers a run's kept key hash again after a restart.
	Register(runID uuid.UUID, hash [32]byte) error
	// Succeed hands the run to a new proxy under a new key, at a create and at a revive alike; it
	// refuses while the run's last proxy is not settled.
	Succeed(runID uuid.UUID, hash [32]byte) error
	ProxyConfig(key string) any
	Drain(ctx context.Context, runID uuid.UUID) error
	ProducerGone(ctx context.Context, runID uuid.UUID) error
}

// EvidenceStore is the runner's per-run record store: Load returns the run's last saved record (nil
// when none) and Save writes it with the offline state's atomic write and fsync.
type EvidenceStore interface {
	LoadEvidence(runID uuid.UUID) ([]byte, error)
	SaveEvidence(runID uuid.UUID, b []byte) error
}

// sinkEvidence is the driver's part of a run's record: the hash of its current proxy's sink key.
type sinkEvidence struct {
	SinkKeyHash string `json:"sink_key_hash,omitempty"`
}

var errDecisionSinkUnwired = errors.New("docker: a runner's proxy needs wardyn-runnerd's decision sink, which is not wired")

// runnerMode reports a Driver wardyn-runnerd built.
func (d *Driver) runnerMode() bool { return d.cfg.RunnerID != "" }

// runnerSinkConfig is the one place a proxy's config gets decision_sink, for a run's create and a
// revive's replace alike. In runner mode the proxy gets a key of its own: its hash is saved in the run's
// record, then the run is handed to it in the sink, both before the proxy exists; a failed save stops the
// create. A received decision_spool_dir never survives, on any substrate: no proxy this driver starts
// shares a directory with runnerd.
func (d *Driver) runnerSinkConfig(runID uuid.UUID, b []byte) ([]byte, error) {
	var data map[string]json.RawMessage
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, err
	}
	if data == nil {
		return nil, errors.New("proxy config is empty")
	}
	_, received := data["decision_spool_dir"]
	delete(data, "decision_spool_dir")
	if !d.runnerMode() {
		if !received {
			return b, nil
		}
		return json.Marshal(data)
	}
	if d.cfg.DecisionSink == nil || d.cfg.Evidence == nil {
		return nil, errDecisionSinkUnwired
	}
	key, hash, err := d.cfg.DecisionSink.NewKey()
	if err != nil {
		return nil, fmt.Errorf("docker: decision sink key: %w", err)
	}
	ev, err := json.Marshal(sinkEvidence{SinkKeyHash: hex.EncodeToString(hash[:])})
	if err != nil {
		return nil, err
	}
	if err := d.cfg.Evidence.SaveEvidence(runID, ev); err != nil {
		return nil, fmt.Errorf("docker: keep the decision sink key: %w", err)
	}
	if err := d.cfg.DecisionSink.Succeed(runID, hash); err != nil {
		return nil, fmt.Errorf("docker: decision sink: %w", err)
	}
	if data["decision_sink"], err = json.Marshal(d.cfg.DecisionSink.ProxyConfig(key)); err != nil {
		return nil, err
	}
	return json.Marshal(data)
}

// sinkKeyHash is the run's kept sink key hash, "" when its proxy has none (one started outside runner
// mode, or by a runnerd from before the sink).
func (d *Driver) sinkKeyHash(runID uuid.UUID) (string, error) {
	b, err := d.cfg.Evidence.LoadEvidence(runID)
	if err != nil || len(b) == 0 {
		return "", err
	}
	var ev sinkEvidence
	if err := json.Unmarshal(b, &ev); err != nil {
		return "", fmt.Errorf("docker: run %s: unreadable sink record: %w", runID, err)
	}
	return ev.SinkKeyHash, nil
}

// drainSink asks the run's proxy, in runner mode, to stop admitting and deliver what it holds before the
// driver stops or removes it. Whatever it answers, the stop goes on.
func (d *Driver) drainSink(ctx context.Context, runID uuid.UUID) {
	if d.runnerMode() && d.cfg.DecisionSink != nil {
		_ = d.cfg.DecisionSink.Drain(ctx, runID)
	}
}

// removeProxy force-removes the run's proxy container, if there is one, then settles its producer: a
// removed container's process has ended, which is completion evidence. A failed remove is an error to
// retry, and settles nothing.
func (d *Driver) removeProxy(ctx context.Context, runID uuid.UUID) error {
	if _, err := d.cli.ContainerRemove(ctx, proxyContainerName(runID), client.ContainerRemoveOptions{Force: true}); err != nil && !isNotFound(err) {
		return fmt.Errorf("docker: remove proxy: %w", err)
	}
	return d.proxyGone(ctx, runID)
}

// proxyGone settles the run's producer in the sink once its proxy container is confirmed gone: what it
// left unacknowledged is written off as loss (exact after a drain), and the spool's writer is released.
func (d *Driver) proxyGone(ctx context.Context, runID uuid.UUID) error {
	if !d.runnerMode() || d.cfg.DecisionSink == nil || d.cfg.Evidence == nil {
		return nil
	}
	hash, err := d.sinkKeyHash(runID)
	if err != nil || hash == "" {
		return err
	}
	if err := d.cfg.DecisionSink.ProducerGone(ctx, runID); err != nil {
		return fmt.Errorf("docker: settle the proxy's decisions: %w", err)
	}
	return nil
}

// ReregisterSinks registers each run's kept sink key hash again. wardyn-runnerd calls it at startup,
// before its startup containment and before Online, so a proxy still running reconnects and delivers
// what it holds while it is stopped.
func (d *Driver) ReregisterSinks(runIDs []uuid.UUID) error {
	if d.cfg.DecisionSink == nil || d.cfg.Evidence == nil {
		return nil
	}
	var errs []error
	for _, id := range runIDs {
		h, err := d.sinkKeyHash(id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if h == "" {
			continue
		}
		b, err := hex.DecodeString(h)
		if err != nil || len(b) != 32 {
			errs = append(errs, fmt.Errorf("docker: run %s: malformed sink key hash", id))
			continue
		}
		if err := d.cfg.DecisionSink.Register(id, [32]byte(b)); err != nil {
			errs = append(errs, fmt.Errorf("docker: re-register run %s with the decision sink: %w", id, err))
		}
	}
	return errors.Join(errs...)
}
