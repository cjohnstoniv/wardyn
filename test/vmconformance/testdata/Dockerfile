# The VM conformance suite's images (test/vmconformance): the hermetic origin
# and the QEMU guest (an Alpine virt kernel and a busybox initramfs whose /init
# runs vmprobe). Built by the suite itself from the repository root.
FROM golang:1.27.2-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build
ENV GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=linux GOARCH=amd64
ARG GOPROXY=
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /out/origin ./test/vmconformance/origin && go build -o /out/vmprobe ./test/vmconformance/vmprobe

FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS guest
RUN apk add --no-cache linux-virt busybox-static cpio
COPY test/vmconformance/testdata/init /initrd/init
COPY --from=build /out/vmprobe /initrd/vmprobe
RUN set -eu; k=/lib/modules/$(ls /lib/modules); \
    mkdir -p /initrd/bin /initrd/lib /initrd/proc /initrd/sys /initrd/dev /out; \
    cp /bin/busybox.static /initrd/bin/busybox; \
    for m in net/core/failover drivers/net/net_failover drivers/net/virtio_net; do \
        gunzip -c "$k/kernel/$m.ko.gz" > "/initrd/lib/${m##*/}.ko"; \
    done; \
    chmod 0755 /initrd/init; \
    (cd /initrd && find . | cpio -o -H newc --quiet | gzip -9) > /out/initrd.gz; \
    cp /boot/vmlinuz-virt /out/vmlinuz

FROM debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a AS vm
RUN apt-get update && apt-get install -y --no-install-recommends qemu-system-x86 && rm -rf /var/lib/apt/lists/*
COPY --from=guest /out/ /vm/
USER 65532:65532
ENTRYPOINT ["qemu-system-x86_64"]

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS origin
COPY --from=build /out/origin /origin
ENTRYPOINT ["/origin"]
