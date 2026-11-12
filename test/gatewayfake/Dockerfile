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
FROM golang:1.27.3-alpine@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS builder
ENV GOTOOLCHAIN=local
WORKDIR /build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/gatewayfake ./test/gatewayfake

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=builder /out/gatewayfake /usr/local/bin/gatewayfake
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/gatewayfake"]
