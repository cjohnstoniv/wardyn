<!-- Copyright 2025 The Wardyn Authors -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# A forward proxy bound to loopback: why sandbox egress can't use it, and the relay that fixes it

**Applies to:** any host whose corporate connectivity client binds its forward proxy to
`127.0.0.1:PORT` rather than a routable address. Common on managed laptops, and the default for
several corporate VPN/connectivity agents.

## Symptom

Everything looks configured. `HTTP_PROXY` is set, `curl` works from your shell, `make setup`
completes, and the Getting-Started checklist is green. Then the first **approved** sandbox egress
does not return — the request just sits there.

Before v0.4.2 it would hang indefinitely. It now fails with a `dial upstream proxy` error after a
bounded handshake timeout, but the underlying cause is the same.

## Why

A loopback address is not a location, it is a *scope*. `127.0.0.1` inside a sandbox is the sandbox's
own loopback; `127.0.0.1` inside a container-runtime VM is the VM's. So a proxy bound only to the
host's loopback is reachable from host processes and **from nothing else**:

```
  host process        -> 127.0.0.1:PORT   OK
  container           -> 127.0.0.1:PORT   its own loopback, nothing there
  container-runtime VM-> <host routable IP>:PORT   the proxy isn't bound there either
```

`wardyn-proxy` therefore has no usable upstream. The request is correctly evaluated and **approved**
— the failure is one layer lower, at the dial.

This is not specific to Wardyn: nothing in a container can reach a host's loopback-only listener.
What *was* specific to Wardyn is that the failure used to be silent, because the MITM path answers
the agent's `CONNECT` with `200 Connection Established` before the upstream dial is attempted. That
is fixed; a stalled upstream is now a fast, logged denial.

## Fix: relay a reachable port to the loopback proxy

```sh
# 1. On the host, in the foreground (Ctrl-C to stop).
wardyn setup proxy-relay 18080 CORP_PROXY_PORT

# 2. Store the relay address as the upstream-proxy secret. <host-gateway> is the
#    address your sandbox reaches this host on — on a VM-backed runtime that is
#    the VM's gateway, not 127.0.0.1.
wardyn secret set upstream-proxy-url          # paste: http://<host-gateway>:18080

# 3. Reference it from the operator-wide site config (or the Host proxy step in
#    Getting Started).
wardyn site-config get > corp-baseline.json   # edit upstream_proxy_secret_ref
wardyn site-config apply corp-baseline.json
```

Verify from inside a run: an approved request to an allowed host should return a real HTTP status
(any status — a round-trip is the signal), and `wardyn-proxy` logs *chaining egress through upstream
proxy*.

### Finding `<host-gateway>`

It depends on your runtime; there is no portable answer. Ask the VM, not the host:

```sh
docker run --rm alpine:3.20 sh -c 'ip route | awk "/default/ {print \$3}"'
```

Then confirm the relay is reachable at that address from the same place:

```sh
docker run --rm alpine:3.20 sh -c 'nc -z -w3 <host-gateway> 18080 && echo reachable'
```

## What the relay is and isn't

- It is a **dumb TCP forwarder**: no TLS, no parsing, no policy. Egress policy is enforced by
  `wardyn-proxy` *before* traffic reaches it, and your corporate proxy's own authentication still
  applies to every request.
- It is **foreground only**. Wardyn supervises no host processes. If you need it across reboots,
  wrap it in your own user-level service (`launchd`, `systemd --user`); keeping that lifecycle
  yours is deliberate.
- It **widens exposure of the corp proxy** to whatever can reach the listen address. Narrow it with
  `--listen-addr` when a specific interface reaches your sandbox.

## If you'd rather not run a relay

- **Rebind the proxy.** If your connectivity client can listen on a routable address or on the VM's
  gateway interface, do that instead — no relay, no extra process.
- **Run wardynd in host mode** (`WARDYN_SETUP_MODE=local`). Host-mode wardynd shares the host's
  network namespace, so it can use the loopback proxy directly. Per-run sandboxes still can't, so
  this only helps control-plane egress.
