# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# adofake — the TEST image for the kind walk's Azure DevOps profile: the Entra
# and Azure DevOps fakes behind one TLS-terminating forward proxy
# (test/adofake/cmd/main.go).
#
# NOT a product image. It is never built by `make agent-images`, never
# published (release.yml does not name it), and it is built only by
# deploy/kind/sso/overlay.sh and `kind load`ed straight into a throwaway
# cluster, as wardyn/test-adofake:local.
#
# Alpine rather than distroless: the Azure DevOps fake serves git through the
# real `git http-backend` (alpine ships it in git-daemon), so git has to be on PATH.
#
# Build from the REPO ROOT:
#   docker build -f test/adofake/cmd/Dockerfile -t wardyn/test-adofake:local .
FROM golang:1.27.2-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS builder
ENV GOTOOLCHAIN=local
WORKDIR /build
COPY go.mod go.sum ./
# Bounded retry of the module FETCH only (a proxy stream error is transient); go.sum still
# verifies every module, and no build or test step is ever retried.
RUN for i in 1 2 3; do go mod download && break; [ "$i" -lt 3 ] || exit 1; sleep $((i*5)); done
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/adofake ./test/adofake/cmd

FROM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
RUN apk add --no-cache git git-daemon && adduser -D -u 65532 adofake
COPY --from=builder /out/adofake /usr/local/bin/adofake
USER 65532
EXPOSE 3128
ENTRYPOINT ["/usr/local/bin/adofake"]
