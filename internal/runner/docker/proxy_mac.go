// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"net/netip"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/network"
)

// proxyMAC is the MAC address every proxy of runID takes on the run's network: at the create, and at every replace
// or revive after it. The agent's neighbour cache maps the proxy's address there to a MAC. An engine that gives each
// new container a random MAC would give each new proxy another one, and a netstack that ignores the gratuitous ARP
// a new interface sends (gVisor) keeps sending to the cached MAC until the entry ages out, up to about a minute.
// The first byte, 0x02, makes it a locally administered unicast address; the other five come from a hash of the run
// id. Only the run's agent and its proxy are on that network, so another run's proxy that hashed to the same MAC
// is on another network.
func proxyMAC(runID uuid.UUID) network.HardwareAddr {
	sum := sha256.Sum256(runID[:])
	return network.HardwareAddr{0x02, sum[0], sum[1], sum[2], sum[3], sum[4]}
}

// proxyStepError is a failure of the proxy container's create or start: the two steps at which an engine takes or
// refuses the MAC address the create names. Docker checks the address only for its format at the create and applies
// it at the start. Its text is the wrapped error's own.
type proxyStepError struct {
	step string // "create" or "start"
	err  error
}

func (e *proxyStepError) Error() string { return e.err.Error() }
func (e *proxyStepError) Unwrap() error { return e.err }

// startProxyOn starts runID's proxy with proxyMAC on the run's network (startProxyWithMAC). When the create or the
// start that names the address fails, what that attempt made is already removed, and the proxy is made once more
// with an address the engine chooses. If the name is still held then, the first failure is the answer. The run
// keeps its egress; its agent waits again after a replace, which the warning says. A name conflict is the
// caller's to handle, a done ctx is not tried again, and a failure at any other step is returned as it is.
func (d *Driver) startProxyOn(ctx context.Context, image string, runID uuid.UUID, labels map[string]string, env []string, prep proxyPrep, ip netip.Addr) (string, string, error) {
	id, sum, err := d.startProxyWithMAC(ctx, image, runID, labels, env, prep, ip, proxyMAC(runID))
	var failed *proxyStepError
	if !errors.As(err, &failed) || isNameConflict(err) || ctx.Err() != nil {
		return id, sum, err
	}
	msg := "docker: creating the run's proxy with its MAC address on the run network failed, so it is created again"
	if failed.step == "start" {
		msg = "docker: starting the run's proxy with its MAC address on the run network failed, so it is removed and created again"
	}
	slog.Warn(msg+" with one the engine chooses; after a replace, a gVisor agent can wait up to a minute before it reaches the new proxy",
		"run", runID, "err", err)
	// What the first attempt made is gone, or never was: settle its producer, as every other second start does, so a
	// runner's sink takes the second attempt's key.
	if gerr := d.proxyGone(ctx, runID); gerr != nil {
		return id, sum, err
	}
	id2, sum2, err2 := d.startProxyWithMAC(ctx, image, runID, labels, env, prep, ip, nil)
	if failed.step == "start" && isNameConflict(err2) {
		// The start's proxy still holds the name, so the second made nothing: the first failure is the answer. A
		// caller that retries a name conflict would take that proxy for another's. After a failed create the
		// name is held by another proxy, and the conflict is the caller's to read.
		return id, sum, err
	}
	return id2, sum2, err2
}
