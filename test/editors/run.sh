#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Entry point of the editors image (see Dockerfile): drives real editor clients
# against a live SSH gateway and prints one "[pass]" / "[FAIL]" line per check.
# Inputs (env): GW_HOST, GW_PORT, GW_USER (the run id), and the owner key at
# /keys/owner_key. Args: the clients to run (mutagen).
set -uo pipefail

FAILED=0
pass() { printf '  [pass] %s\n' "$1"; }
fail() { printf '  [FAIL] %s\n' "$1"; FAILED=1; }

mkdir -p ~/.ssh && chmod 700 ~/.ssh
install -m 600 /keys/owner_key ~/.ssh/owner_key
# Host alias for the clients that take only a host name; the user is the run id.
cat > ~/.ssh/config <<CFG
Host sandbox
  HostName ${GW_HOST}
  Port ${GW_PORT}
  User ${GW_USER}
  IdentityFile ~/.ssh/owner_key
  IdentitiesOnly yes
  BatchMode yes
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  LogLevel ERROR
CFG
remote() { ssh sandbox "$@"; }

client_mutagen() {
  export PATH="/opt/mutagen:${PATH}"
  mutagen version | sed 's/^/  mutagen /'
  mkdir -p /work/alpha
  head -c 1048576 /dev/urandom > /work/alpha/blob.bin
  echo "from-local" > /work/alpha/local.txt
  local want
  want="$(sha256sum /work/alpha/blob.bin | cut -d' ' -f1)"

  # Mutagen installs its agent under $HOME, then runs it by the relative path
  # .mutagen/agents/... A real sshd starts an exec in $HOME; the gateway starts
  # it in the sandbox's working directory (/home/agent/work), so without this
  # link the run fails with ".mutagen/agents/0.18.1/mutagen-agent: not found".
  remote 'mkdir -p ~/.mutagen && ln -sfn ~/.mutagen .mutagen'
  if ! mutagen sync create --name editors-alpha --sync-mode two-way-resolved \
      /work/alpha sandbox:/home/agent/alpha 2>&1; then
    fail "mutagen: sync create over the gateway"
    # Where an exec channel starts, against where Mutagen installed its agent.
    remote 'echo "exec cwd: $(pwd)  HOME: ${HOME}"; ls -ld .mutagen/agents/0.18.1 ~/.mutagen/agents/0.18.1' 2>&1 | sed 's/^/    /'
    return
  fi
  pass "mutagen: sync create over the gateway (agent installed through it)"

  if ! mutagen sync flush editors-alpha 2>&1; then
    fail "mutagen: sync flush"
    mutagen sync list editors-alpha 2>&1 | sed 's/^/    /'
    return
  fi
  local got
  got="$(remote "sha256sum /home/agent/alpha/blob.bin" | cut -d' ' -f1)"
  if [[ "${got}" == "${want}" && "$(remote 'cat /home/agent/alpha/local.txt')" == "from-local" ]]; then
    pass "mutagen: local files reached the sandbox byte-identical after flush"
  else
    fail "mutagen: sandbox copy differs (sha256 '${got}', want '${want}')"
  fi

  remote "echo from-sandbox > /home/agent/alpha/remote.txt"
  mutagen sync flush editors-alpha >/dev/null 2>&1
  if [[ "$(cat /work/alpha/remote.txt 2>/dev/null)" == "from-sandbox" ]]; then
    pass "mutagen: a file written in the sandbox came back after flush"
  else
    fail "mutagen: sandbox-written file did not sync back"
  fi

  if mutagen sync list editors-alpha | grep -q 'Status: Watching for changes' \
      && ! mutagen sync list editors-alpha | grep -qiE 'conflicts|problems'; then
    pass "mutagen: session settled at 'Watching for changes' with no conflicts or problems"
  else
    fail "mutagen: session not settled cleanly"
    mutagen sync list editors-alpha 2>&1 | sed 's/^/    /'
  fi
  mutagen sync terminate editors-alpha >/dev/null 2>&1
  mutagen daemon stop >/dev/null 2>&1
}

# JetBrains: the half of Gateway's flow that runs on the remote, which is the
# part that rides the SSH gateway. Gateway uploads the IDE over sftp, unpacks it
# with an exec, starts the headless remote-dev-server, and forwards its port to
# the thin client. This does the same with the same files; the thin client
# itself (a GUI) is the one piece not run here. IntelliJ IDEA 2026.2.3.
JB_VERSION=2026.2.3
JB_SHA256=68751c8ae4d49407251cd197df795fbed91b6fdc85d10c73c4649a99e496ab37
client_jetbrains() {
  local archive="/cache/idea-${JB_VERSION}.tar.gz"
  mkdir -p /cache
  [[ -f "${archive}" ]] || curl -fsSL -o "${archive}" "https://download.jetbrains.com/idea/idea-${JB_VERSION}.tar.gz"
  if ! echo "${JB_SHA256}  ${archive}" | sha256sum -c --quiet -; then
    fail "jetbrains: IDE archive does not match the pinned sha256"
    return
  fi

  if sftp -b - sandbox >/tmp/jb-sftp.log 2>&1 <<SFTP; then
put ${archive} /home/agent/idea.tar.gz
SFTP
    pass "jetbrains: IDE archive uploaded over sftp ($(du -m "${archive}" | cut -f1) MiB)"
  else
    fail "jetbrains: sftp upload: $(tail -5 /tmp/jb-sftp.log)"
    return
  fi
  if remote 'mkdir -p ~/jb ~/proj && tar -xzf ~/idea.tar.gz -C ~/jb && rm ~/idea.tar.gz && echo hi > ~/proj/a.txt' >/tmp/jb-unpack.log 2>&1; then
    pass "jetbrains: IDE unpacked in the sandbox over an exec channel"
  else
    fail "jetbrains: unpack over exec: $(tail -5 /tmp/jb-unpack.log)"
    return
  fi

  # Detached, as the gateway's exec contract requires of anything left running.
  remote 'cd ~/jb/idea-*/bin && REMOTE_DEV_NON_INTERACTIVE=1 REMOTE_DEV_TRUST_PROJECTS=1 \
    nohup ./remote-dev-server.sh run ~/proj >~/rds.log 2>&1 </dev/null & sleep 1; echo started'
  local link=""
  for _ in $(seq 90); do
    link="$(remote "grep -m1 '^Join link' ~/rds.log" 2>/dev/null)" && [[ -n "${link}" ]] && break
    sleep 2
  done
  if [[ -z "${link}" ]]; then
    fail "jetbrains: remote-dev-server printed no join link"
    remote 'tail -30 ~/rds.log' 2>&1 | sed 's/^/    /'
    return
  fi
  pass "jetbrains: remote-dev-server started headless in the sandbox (${link%%#*})"

  local port lport
  port="$(sed -E 's|^Join link: tcp://127.0.0.1:([0-9]+)#.*|\1|' <<<"${link}")"
  lport=$((20000 + RANDOM % 20000))
  ssh -N -L "${lport}:127.0.0.1:${port}" sandbox &
  local fwd=$!
  local subject=""
  for _ in $(seq 15); do
    subject="$(echo | openssl s_client -connect "127.0.0.1:${lport}" 2>/dev/null | openssl x509 -noout -subject 2>/dev/null)" && [[ -n "${subject}" ]] && break
    sleep 1
  done
  kill "${fwd}" 2>/dev/null
  if [[ -n "${subject}" ]]; then
    pass "jetbrains: TLS handshake with the backend through -L (${subject})"
  else
    fail "jetbrains: no TLS handshake with the backend through the -L forward"
  fi
}

for c in "$@"; do
  echo "[client] ${c}"
  "client_${c}"
done
exit "${FAILED}"
