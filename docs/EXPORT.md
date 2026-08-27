# Export control

Wardyn is published from the United States and contains cryptographic
functionality. This page states its classification so an intake form can be
completed without guesswork — a blank ECCN field fails many procurement forms
before a human reads them.

## Self-classification

| field | value |
|---|---|
| ECCN | **5D002** (information security software) |
| EAR status | **Not subject to the EAR** — §742.15(b)(1), publicly available encryption source code |
| Licence required | **No** |
| CCATS | Not required; no classification request was submitted |
| Encryption registration | Not required for publicly available source under §742.15(b) |

The source code is published without charge at
`https://github.com/cjohnstoniv/wardyn` and is available to the general public,
which is the condition §742.15(b) turns on.

## Notification

**None is required.** This page previously recorded a pending BIS/NSA notification. That
obligation does not apply to Wardyn, and the entry was wrong.

§742.15(b)(1) places publicly available 5D002 encryption source code outside the EAR outright.
The email notification to BIS and the ENC Encryption Request Coordinator was removed as a general
condition by BIS's final rule of 29 March 2021; what survives is §742.15(b)(2), which requires it
**only** for source code that provides or performs *"non-standard cryptography."*

Part 772 defines that term as *"any implementation of 'cryptography' involving the incorporation or
use of proprietary or unpublished cryptographic functionality, including encryption algorithms or
protocols that have not been adopted or approved by a duly recognized international standards body
(e.g., IEEE, IETF, ISO, ITU, ETSI, 3GPP, TIA, and GSMA) and have not otherwise been published."*

Wardyn contains none. Every primitive in the inventory below is a published algorithm adopted by a
recognised standards body and consumed from a third-party library; Wardyn implements no
cryptographic algorithm of its own and modifies none. §742.15(b)(2) is therefore not triggered.

- Applicable notification: **none.**
- **Re-open this if that ever stops being true** — a bespoke construction, a modified primitive, or
  an unpublished protocol would trigger §742.15(b)(2). The recipients would then be
  `crypt@bis.doc.gov` and `enc@nsa.gov`, notified of `https://github.com/cjohnstoniv/wardyn`.

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
