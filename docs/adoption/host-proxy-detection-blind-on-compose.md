<!-- Copyright 2025 The Wardyn Authors -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Host-proxy detection is blind on the containerized control plane

**Date:** 2026-07-20
**Severity:** low-impact, high-confusion. Not a security defect; a **false negative
in a diagnostic** that undermines trust in the whole Getting-Started checklist for
exactly the users the corporate path targets.
**Status:** fixed in 0.4.1.

## Symptom

On the containerized control plane, Getting Started → **Corporate host proxy**
reports:

> Host proxy — No host-side proxy configuration detected (env vars, shell profiles,
> git config, tool configs, or OS proxy settings).

…on a host that is unambiguously behind a corporate proxy. In the same session the
operator's shell has `HTTP_PROXY`/`HTTPS_PROXY` exported, and Wardyn's **own image
build used that proxy** (compose forwards it as `--build-arg HTTP_PROXY=…`). So
Wardyn demonstrably knows the proxy exists at build time, yet the runtime detector
says "nothing here."

## Root cause

`internal/setup/detect_proxy.go`'s `DetectHostProxy()` runs **in the wardynd
process**, and on the containerized stack that process is a distroless container
started with no proxy environment and no view of the host. Every detection tier is
structurally blind there:

1. **env tier** — `detectEnvProxyCandidates()` reads the *process's own* env.
   Compose forwards `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` as **`build.args` only**,
   never in the wardynd service's runtime `environment:` block. Confirmed live:
   `docker inspect` on the running container shows no proxy vars in `.Config.Env`.
2. **shell-profile tier** — `shellProfilePaths(home)` reads `~/.bashrc`/`.zshrc`/
   `.profile` + `/etc/environment` **inside the container**. The operator's real
   profiles are on the host. Worse than it looks: `HOME` is unset in the image, so
   `os.UserHomeDir()` returns `""` and all four dotfiles are skipped outright.
3. **OS tier** — `detectOSProxyCandidates()` dispatches on `runtime.GOOS`: `scutil`
   on darwin, the Windows registry via WSL, else nothing. In the container
   `GOOS == "linux"`, so the `default:` branch returns nothing. A macOS operator's
   actual setting is often a **PAC file** (`ProxyAutoConfigEnable: 1`,
   `ProxyAutoConfigURLString: …`, `HTTPEnable: 0`), which the darwin path *does*
   surface as `HostProxyPAC` — but only when the detector runs **on the macOS host**.

Two further tiers the original report missed, both also dead in-container:

4. **git tier** — shells out to `git config --global`; the distroless runtime image
   ships no `git` binary, so it always returns nil.
5. **tool-config tier** — `~/.npmrc`, `~/.config/pip/pip.conf`, `~/.cargo/config.toml`,
   `~/.m2/settings.xml` are all gated behind `home != ""`.

So the empty-state copy enumerated **five** mechanisms as checked when none of them
could be read.

The detector itself is good (PAC/WPAD aware, WSL registry, tool configs, credential
masking). The bug is **location**: it introspects the wrong machine. In host mode
(`WARDYN_SETUP_MODE=local`) it works; on the containerized default it cannot.

**Why it matters:** `make setup` is containerized by default as of 0.4, and the
corporate-network story is a headline 0.4 feature. So the default deployment for the
exact audience that has a corporate proxy is the one where proxy detection silently
returns a false negative — teaching users the checklist is unreliable right where
they need it most.

## What closed it

**Detect on the host and seed the result in.** The image now also cross-compiles a
host-native `wardyn` (`Dockerfile.wardynd`, `HOST_GOOS`/`HOST_GOARCH` build args, a
few lines given the pure-Go build). `scripts/up.sh` extracts it, runs the new
`wardyn setup detect-proxy` **on the host** — where every tier above is live — and
seeds the JSON into the compose env as `WARDYN_HOST_PROXY_B64`. `DetectHostProxy()`
returns the seed when present. This recovers the PAC/OS/shell/git/tool tiers that
are inherently host-side, and re-runs on every `up`, so it cannot go stale across a
network change.

**Honest copy when it genuinely can't look.** When wardynd is containerized *and* no
seed is present, the empty result no longer asserts a false negative. It states that
detection ran inside the container, names what it therefore could not read, and
carries a `Fix` line. The static UI lede ("Wardyn detected these host proxy
settings…"), which rendered unconditionally above an empty result, was deleted.

**Rejected: forwarding the standard `HTTP_PROXY` names into wardynd's runtime env.**
That was the original proposal, and it is unsafe. Go's `net/http` honors those names
process-wide via `ProxyFromEnvironment`, so setting them would silently reroute
wardynd's *own* outbound calls — OIDC discovery to the in-network identity service
(Go exempts only loopback, never a bare service name), the audit webhook sink,
GitHub App token minting, the AWS credential chain — through the corporate proxy. A
live-traffic change to fix a diagnostic string. The repo already states this
invariant in `internal/composer/backends/transport/transport.go` (`Proxy: nil`) and
in `Dockerfile.wardynd` ("build-only ARGs, NOT ENV"). The seed var is deliberately
WARDYN-namespaced so `net/http` never reads it.

Also fixed while here: a **set-but-empty** `HTTP_PROXY` counted as a detected proxy
(`os.LookupEnv` reports ok for `export HTTP_PROXY=`, and every consumer tested
presence only), rendering a blank-valued "detected" row. Empty and whitespace-only
values are now filtered — otherwise the fix above would have traded a false negative
for a false positive.

## Acceptance test

On a host behind a corporate proxy (env var and/or PAC), running the **containerized**
stack: Getting Started → Corporate host proxy either (a) shows the detected proxy,
or (b) explicitly states detection ran inside the container and names the host-mode /
manual alternative — and in **no** case claims "no proxy configuration detected" when
`HTTP_PROXY` is set in the operator's shell that launched `make setup`.

Verified live on both paths: with the seed present the step reports the host's real
proxy; with the seed removed it reports the honest "couldn't look there" copy plus a
next step.

## Note

Functionally nothing was blocked: the step is optional and governs only whether the
sandbox chains its egress through the corporate proxy — the operator can set
`upstream-proxy-url` by hand and move on, and model access does not depend on it.
This was purely about the diagnostic telling the truth so the corporate-network
onboarding checklist is trustworthy end to end.
