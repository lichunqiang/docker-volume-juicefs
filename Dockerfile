FROM golang:1.23 as builder

ARG GOPROXY
ENV GOPROXY=${GOPROXY:-"https://proxy.golang.org,direct"}
ARG JUICEFS_CE_VERSION
ENV JUICEFS_CE_VERSION=${JUICEFS_CE_VERSION:-"1.2.3"}

WORKDIR /docker-volume-juicefs
COPY . .
RUN apt-get update && apt-get install -y curl musl-tools tar gzip librados-dev

RUN CC=/usr/bin/musl-gcc go build -o bin/docker-volume-juicefs --ldflags '-linkmode external -extldflags "-static"' .

WORKDIR /workspace
# ce
RUN curl -fsSL -o juicefs-ce.tar.gz https://github.com/juicedata/juicefs/archive/refs/tags/v${JUICEFS_CE_VERSION}.tar.gz && \
    tar -zxf juicefs-ce.tar.gz -C /tmp && \
    cd /tmp/juicefs-${JUICEFS_CE_VERSION} && \
    make juicefs.ceph && \
    mv juicefs.ceph /tmp/juicefs

FROM python:3.12-slim

RUN apt-get update && apt-get install -y ceph-common && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /run/docker/plugins /jfs/state /jfs/volumes
COPY --from=builder /docker-volume-juicefs/bin/docker-volume-juicefs /
COPY --from=builder /tmp/juicefs /usr/bin/
RUN /usr/bin/juicefs version 
CMD ["docker-volume-juicefs"]