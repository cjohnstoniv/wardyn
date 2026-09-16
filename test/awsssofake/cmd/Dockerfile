# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# awsssofake — the TEST image for the kind SSO walk's fake AWS endpoints.
#
# NOT a product image. It is never built by `make agent-images`, never
# published, and carries none of the licence-distribution ceremony the shipped
# images carry (deploy/compose/Dockerfile.proxy) precisely because it is never
# distributed: it is built by deploy/kind/sso/overlay.sh and `kind load`ed
# straight into a throwaway cluster.
#
# Build from the REPO ROOT:
#   docker build -f test/awsssofake/cmd/Dockerfile -t wardyn/awsssofake:local .
FROM golang:1.27-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS builder
ENV GOTOOLCHAIN=local
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/awsssofake ./test/awsssofake/cmd

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=builder /out/awsssofake /usr/local/bin/awsssofake
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/awsssofake"]
