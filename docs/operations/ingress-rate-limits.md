> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Behind a shared ingress

The rate limits on the routes open to callers with no credential, and `WARDYN_TRUSTED_PROXIES`, which decides whose address each limit counts. They matter when runners and laptops reach the organisation through one ingress ([hybrid laptops](hybrid-laptops.md)).

## Rate limits

Three routes are open to callers with no credential yet. Each has its own rate-limit bucket per client address: 10 attempts, then one every 10 seconds. The address is the TCP peer unless `WARDYN_TRUSTED_PROXIES` lists that peer, so without it every caller behind a shared ingress, internet traffic included, shares the ingress's bucket.

| Route | Refusal when the bucket is empty |
| --- | --- |
| `/api/v1/runners/{id}/stream` | `429` `runner_stream_rate_limited` |
| `/api/v1/runners/register` | `429` `runner_registration_rate_limited` |
| `/api/v1/devices/enrol` | `429` `device_enrol_rate_limited` |

- The runner stream charges its bucket only when a handshake ends unproved; a registration that redeems a valid token is refunded. Failed and abandoned attempts still drain the bucket, so without trusted proxies one peer can lock every runner out.
- Reconnecting runners stay reliable only if you do one of these. Set `WARDYN_TRUSTED_PROXIES` to your ingress addresses so each client gets its own bucket. Or allow-list your runners' source networks on `/api/v1/runners/*/stream` and the other two paths. Or give runners a dedicated ingress or hostname. Per-client limits at the ingress do not close it on their own.
- The buckets are per replica, so an HA install's effective limit is the per-replica limit times the replica count.

The runner stream has two more anonymous limits and one per-runner limit, all per replica:

| Limit | Value | Spent by |
| --- | --- | --- |
| Global handshake bucket | 512, then 10 a second, shared by every address | Handshakes that end unproved |
| Handshakes in progress per address | 32 (`WARDYN_RUNNER_STREAM_HANDSHAKES_PER_ADDRESS`) | As below, from one address |
| Handshakes in progress | 256 connections between arrival and proof | A connection until it proves its key or closes; the 15-second handshake deadline bounds each |
| Per-runner reconnects | 10, then one every 10 seconds, per runner | Only a runner that has proved its key, so a caller naming a runner it cannot prove never spends that runner's budget |

- Every refusal is `429` `runner_stream_rate_limited`; the message says which limit refused.
- Memory per replica: each keyed bucket map holds at most 4096 entries of under 128 bytes, and each handshake slot under 64 KiB, so the cap bounds unproved connections under 16 MiB.

## Trusted proxies

`WARDYN_TRUSTED_PROXIES` (off by default) lists the proxies whose `X-Forwarded-For` Wardyn reads. It applies to the public listener only; the proxy-facing listener runs dial is never read through it.

1. Wardyn reads the header only when the TCP peer is inside the list.
2. It walks the chain right to left, skipping addresses inside the list; the first address outside it is the client. Entries left of it were written by that client and are never read.
3. A malformed, loopback or unspecified entry, more than 32 entries or more than 2048 bytes, or a chain made only of listed addresses, keeps the TCP peer.
4. The client becomes the row's `source_ip`, and the peer is kept as `data.proxy_peer` on every audit row the request writes (`data.device_origin.proxy_peer` on a device-forwarded row).

- List only your proxies' addresses. Any address in the list can name the client, so a range that holds callers lets them choose their own `source_ip`.
- Your proxy must append the client to `X-Forwarded-For`, or replace the header, never pass a client's value through alone.
