# Export control

Wardyn is published from the United States and contains cryptographic
functionality. This page states its classification so an intake form can be
completed without guesswork — a blank ECCN field fails many procurement forms
before a human reads them.

## Self-classification

| field | value |
|---|---|
| ECCN | **5D002** (information security software) |
| Licence exception | **EAR §742.15(b)** — publicly available encryption source code |
| Licence required | **No** |
| CCATS | Not required; no classification request was submitted |
| Encryption registration | Not required for publicly available source under §742.15(b) |

The source code is published without charge at
`https://github.com/cjohnstoniv/wardyn` and is available to the general public,
which is the condition §742.15(b) turns on.

## Notification

EAR §742.15(b) requires that BIS and the NSA be notified of the internet location
of publicly available encryption source code.

- Recipients: `crypt@bis.doc.gov`, `enc@nsa.gov`
- URL notified: `https://github.com/cjohnstoniv/wardyn`
- Date notified: **PENDING — not yet sent.**

This is tracked as an open item. The classification above is unaffected by it, but
the notification is a real obligation and this file will record the date once it
has been sent, rather than implying it has been.

## Cryptographic inventory

Wardyn implements **no cryptographic algorithms of its own**. Every primitive is a
standard published algorithm consumed from a third-party library:

| use | component |
|---|---|
| Secret store encryption at rest | `filippo.io/age` (X25519, ChaCha20-Poly1305) |
| Transport security | Go standard library `crypto/tls` (TLS 1.2/1.3) |
| Egress-proxy TLS interception | `crypto/x509` — the proxy mints a local CA and leaf certificates for hosts it is configured to inspect |
| SSH gateway | `golang.org/x/crypto/ssh` |
| Identity / SSO | `github.com/coreos/go-oidc/v3`, `github.com/go-jose/go-jose/v4` (JOSE, JWT) |
| Signing, hashing, key handling | `crypto/{ecdsa,ed25519,rsa,elliptic,hmac,sha256,sha1,subtle,rand}` |

Note for reviewers: the TLS-intercepting proxy is a deliberate, documented product
capability, not an artefact — it is how egress policy is enforced. It generates
certificates locally; no key material leaves the deployment.

## Sanctions and field of use

Apache-2.0 imposes **no** field-of-use restriction, no ethical-source clause, and
no geographic restriction. The project adds none. Distribution is subject only to
the controls GitHub and GHCR apply to their own services, and to the export and
sanctions law applying to you as the recipient.
