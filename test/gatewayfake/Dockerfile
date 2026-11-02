# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# gatewayfake — the TEST image behind the AI gateway in the nightly gateway job.
#
# NOT a product image. It is never built by `make agent-images`, never published, and carries none
# of the licence-distribution ceremony the shipped images carry, because it is never distributed:
# scripts/kind-gateway-run.sh builds it and `kind load`s it into a throwaway cluster.
#
# Build from the REPO ROOT:
#   docker build -f test/gatewayfake/Dockerfile -t wardyn/gatewayfake:local .
FROM golang:1.27.2-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS builder
ENV GOTOOLCHAIN=local
WORKDIR /build
COPY go.mod go.sum ./
# Bounded retry of the module FETCH only (a proxy stream error is transient); go.sum still
# verifies every module, and no build or test step is ever retried.
RUN for i in 1 2 3; do go mod download && break; [ "$i" -lt 3 ] || exit 1; sleep $((i*5)); done
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/gatewayfake ./test/gatewayfake

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=builder /out/gatewayfake /usr/local/bin/gatewayfake
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/gatewayfake"]
