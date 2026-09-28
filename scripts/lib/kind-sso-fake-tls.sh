#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Sourced by scripts/kind-sso-walk.sh: the fake AWS endpoints on TLS (#703).
#
# A plain-http fake never exercised the lane it stands in for: over http the
# sandbox's portal call is a plain forward the proxy never terminates, so the
# portal MITM (isMITMHost's third reason, the SSO bearer set on the wire) and
# the hold's timeout body ran nowhere. A throwaway CA, minted fresh each walk
# and never kept (its key is deleted once the leaf is signed), signs a serving
# cert for every name the fake is reached by: the Service names in-cluster, and
# 127.0.0.1 for the harness's port-forwards. The CA reaches every client
# through the one production path, the chart's trustedCA
# (WARDYN_TRUSTED_CA_FILE): wardynd's own portal calls, each run's proxy
# upstream dials, and each sandbox's CA trust (installSandboxTrustedCA); the
# harness trusts it through NODE_EXTRA_CA_CERTS and curl --cacert.
#
# Reads the walk's FAKE_TLS_DIR, FAKE_CA, FAKE_HOST, FAKE_SVC, NAMESPACE and
# CONTEXT; dies through the walk's own die().
fake_tls_up() {
  rm -rf "${FAKE_TLS_DIR}" && mkdir -p "${FAKE_TLS_DIR}"
  if ! { openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=wardyn kind-sso walk CA" \
      -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" \
      -keyout "${FAKE_TLS_DIR}/ca.key" -out "${FAKE_CA}" >/dev/null 2>&1 \
    && openssl req -newkey rsa:2048 -nodes -subj "/CN=${FAKE_HOST}" \
      -keyout "${FAKE_TLS_DIR}/tls.key" -out "${FAKE_TLS_DIR}/tls.csr" >/dev/null 2>&1 \
    && printf 'subjectAltName=DNS:%s,DNS:%s,DNS:%s.%s,DNS:%s.%s.svc,DNS:localhost,IP:127.0.0.1\nextendedKeyUsage=serverAuth\nbasicConstraints=CA:FALSE\n' \
      "${FAKE_HOST}" "${FAKE_SVC}" "${FAKE_SVC}" "${NAMESPACE}" "${FAKE_SVC}" "${NAMESPACE}" >"${FAKE_TLS_DIR}/ext.cnf" \
    && openssl x509 -req -in "${FAKE_TLS_DIR}/tls.csr" -CA "${FAKE_CA}" -CAkey "${FAKE_TLS_DIR}/ca.key" \
      -CAcreateserial -days 2 -extfile "${FAKE_TLS_DIR}/ext.cnf" -out "${FAKE_TLS_DIR}/tls.crt" >/dev/null 2>&1; }; then
    die "could not mint the fake's TLS material (openssl) in ${FAKE_TLS_DIR}"
  fi
  rm -f "${FAKE_TLS_DIR}/ca.key" "${FAKE_TLS_DIR}/tls.csr" "${FAKE_TLS_DIR}/ca.srl"
  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" create secret tls "${FAKE_SVC}-tls" \
    --cert="${FAKE_TLS_DIR}/tls.crt" --key="${FAKE_TLS_DIR}/tls.key" --dry-run=client -o yaml \
    | kubectl --context "${CONTEXT}" -n "${NAMESPACE}" apply -f - >/dev/null \
    || die "could not store the fake's serving cert as secret ${FAKE_SVC}-tls"
  rm -f "${FAKE_TLS_DIR}/tls.key"
  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" patch deployment "${FAKE_SVC}" --type=strategic -p "$(jq -cn --arg s "${FAKE_SVC}-tls" \
    '{spec:{template:{spec:{volumes:[{name:"fake-tls",secret:{secretName:$s}}],
      containers:[{name:"awsssofake",volumeMounts:[{name:"fake-tls",mountPath:"/etc/awsssofake-tls",readOnly:true}]}]}}}}')" >/dev/null \
    || die "could not mount the serving cert into deployment/${FAKE_SVC}"
}
