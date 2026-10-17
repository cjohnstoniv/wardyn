# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# The K6 kind harness's fake organisation (test/k6/fakeorg). Built only by
# scripts/k6-kind-conformance.sh, from the repository root; never published.
FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine@sha256:85dc1069ac644ea3c527b177303a406eb3358192816cd7f9e5848eb658851673 AS builder
WORKDIR /build
ENV GOTOOLCHAIN=local
ARG GOPROXY=
ENV GOPROXY=${GOPROXY}
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -o /out/fakeorg ./test/k6/fakeorg

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=builder /out/fakeorg /fakeorg
USER 65532:65532
ENTRYPOINT ["/fakeorg"]
