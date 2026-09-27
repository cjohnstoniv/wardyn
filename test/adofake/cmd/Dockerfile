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
FROM golang:1.27-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS builder
ENV GOTOOLCHAIN=local
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/adofake ./test/adofake/cmd

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
RUN apk add --no-cache git git-daemon && adduser -D -u 65532 adofake
COPY --from=builder /out/adofake /usr/local/bin/adofake
USER 65532
EXPOSE 3128
ENTRYPOINT ["/usr/local/bin/adofake"]
