ARG REG=docker.io
FROM $REG/golang:1.19 AS builder

ARG GO_LDFLAGS
ARG TARGETARCH

WORKDIR /code
COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH GO_LDFLAGS=$GO_LDFLAGS make WHAT=edgemesh-agent


ARG REG=docker.io
FROM $REG/alpine:3.11

RUN apk update && apk --no-cache add iptables && apk --no-cache add dpkg

COPY --from=builder /code/_output/local/bin/edgemesh-agent /usr/local/bin/edgemesh-agent

# Add support for auto-detection of iptables mode. Every backend must retain
# save/restore slaves when the wrapper switches the selected alternative.
COPY --from=builder /code/build/agent/iptables-wrapper /sbin/iptables-wrapper
RUN update-alternatives --install /sbin/iptables iptables /sbin/iptables-legacy 50 \
    --slave /sbin/iptables-save iptables-save /sbin/iptables-legacy-save \
    --slave /sbin/iptables-restore iptables-restore /sbin/iptables-legacy-restore
RUN update-alternatives --install /sbin/iptables iptables /sbin/iptables-nft 50 \
    --slave /sbin/iptables-save iptables-save /sbin/iptables-nft-save \
    --slave /sbin/iptables-restore iptables-restore /sbin/iptables-nft-restore
RUN update-alternatives --install /sbin/iptables iptables /sbin/iptables-wrapper 100 \
    --slave /sbin/iptables-restore iptables-restore /sbin/iptables-wrapper \
    --slave /sbin/iptables-save iptables-save /sbin/iptables-wrapper

ENTRYPOINT ["edgemesh-agent"]
