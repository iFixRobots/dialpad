FROM golang:1-alpine3.21 AS builder

RUN apk add --no-cache git ca-certificates build-base

COPY . /build
WORKDIR /build
RUN ./build.sh

FROM alpine:3.21

ENV UID=1337 \
    GID=1337

RUN apk add --no-cache su-exec ca-certificates bash jq curl yq-go

COPY --from=builder /build/dialpad-bridge /usr/bin/dialpad-bridge
COPY --from=builder /build/docker-run.sh /docker-run.sh
VOLUME /data

CMD ["/docker-run.sh"]
