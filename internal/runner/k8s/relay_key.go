// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// relayKeyLen is the size of a run's relay key: the proxy sends it in the preamble that opens each
// connection it makes to the control-plane host, and the relay admits that connection as the run
// only when the key is the run's.
const relayKeyLen = 32

// relayPreambleRelease is the first release whose proxy sends the relay preamble when its config
// carries relay_key. The proxy decodes its config strictly, so an older one handed relay_key would
// refuse to start.
const relayPreambleRelease = "0.11.0-rc.1"

// relayKeyed reports whether a run's proxy gets a relay key: runner mode, a relay that checks keys
// (RelayKey wired), and a proxy image that is a known release at or after relayPreambleRelease. An
// image whose release cannot be read (a digest-only pin, a development build) is not judged and
// keeps the address rule, as an older release does.
func (d *Driver) relayKeyed() bool {
	if !d.runnerMode() || d.cfg.RelayKey == nil {
		return false
	}
	tag := imageReleaseTag(d.cfg.ProxyImage)
	return tag != "" && !releaseOlder(tag, relayPreambleRelease)
}

// openRelayKey makes the run's relay key, persists its hash in the run's record, then hands the hash
// to the relay, all before the proxy pod exists. A failed save stops the create and hands nothing.
// The key itself is never stored: it goes only into the proxy's config Secret, as base64.
func (d *Driver) openRelayKey(runID uuid.UUID) (string, error) {
	var key [relayKeyLen]byte
	if _, err := rand.Read(key[:]); err != nil {
		return "", fmt.Errorf("k8s: relay key: %w", err)
	}
	hash := sha256.Sum256(key[:])
	if _, err := d.mutate(runID, func(ev *runEvidence) (bool, error) {
		ev.RelayKeyHash = hex.EncodeToString(hash[:])
		return true, nil
	}); err != nil {
		return "", err
	}
	d.cfg.RelayKey(runID, hash)
	return base64.StdEncoding.EncodeToString(key[:]), nil
}

// relayKeyHash is the run's persisted relay key hash, when its record holds a well-formed one.
func relayKeyHash(ev *runEvidence) ([32]byte, bool) {
	var h [32]byte
	b, err := hex.DecodeString(ev.RelayKeyHash)
	if err != nil || len(b) != len(h) {
		return h, false
	}
	copy(h[:], b)
	return h, true
}

// relayIPRefusal is the create check on a new proxy pod's IP (runner mode): an IP a fenced run still
// holds refuses the run, unless this create keyed its proxy, the runner still keys proxies, and
// keyedOnFencedIP admits it. The relay still refuses an unkeyed connection from the fenced IP.
func (d *Driver) relayIPRefusal(runID uuid.UUID, proxyIP string, keyed bool) error {
	if !d.runnerMode() || d.cfg.IPFenced == nil || !d.cfg.IPFenced(proxyIP) {
		return nil
	}
	if keyed && d.relayKeyed() && d.keyedOnFencedIP(runID, proxyIP) {
		return nil
	}
	return fmt.Errorf("k8s: proxy pod %s: %w", proxyIP, errRelayIPFenced)
}

// keyedOnFencedIP reports whether a keyed run may start on an IP fenced runs still hold: its record
// holds a well-formed relay key hash and its proxy's node, and every run fenced on the IP recorded
// its own proxy on that same node. Only the same node is admitted: with cluster-wide IPAM the IP can
// be handed to a pod on another node while the fenced proxy still runs, and the new run's agent,
// sent to that IP, could reach it. A different or unknown node, or a record that cannot be read,
// refuses.
func (d *Driver) keyedOnFencedIP(runID uuid.UUID, proxyIP string) bool {
	if d.cfg.FencedRuns == nil {
		return false
	}
	ev, err := d.loadEvidence(runID)
	if err != nil {
		return false
	}
	node := proxyNode(ev)
	if _, ok := relayKeyHash(ev); !ok || node == "" {
		return false
	}
	fenced := d.cfg.FencedRuns(proxyIP)
	if len(fenced) == 0 {
		return false
	}
	for _, f := range fenced {
		if f == runID {
			return false
		}
		fev, err := d.loadEvidence(f)
		if err != nil || proxyNode(fev) != node {
			return false
		}
	}
	return true
}

// proxyNode is the node the run's record holds for its proxy pod, "" when none is recorded.
func proxyNode(ev *runEvidence) string {
	if p := ev.Pods[componentProxy]; p != nil {
		return p.NodeName
	}
	return ""
}

// ReplayRelayKeys hands the relay each run's persisted relay key hash again. runnerd calls it at
// startup before Online, so a keyed proxy reconnecting after a restart is admitted. A fenced run's
// hash is left out: the relay refuses that run whatever key it sends.
func (d *Driver) ReplayRelayKeys(runIDs []uuid.UUID) error {
	if d.cfg.RelayKey == nil {
		return nil
	}
	var errs []error
	for _, id := range runIDs {
		ev, err := d.loadEvidence(id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ev.RelayKeyHash == "" || ev.Fence != nil {
			continue
		}
		h, ok := relayKeyHash(ev)
		if !ok {
			errs = append(errs, fmt.Errorf("k8s: run %s: malformed relay key hash", id))
			continue
		}
		d.cfg.RelayKey(id, h)
	}
	return errors.Join(errs...)
}
