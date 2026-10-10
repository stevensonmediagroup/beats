# syntax=docker/dockerfile:1
FROM golang:1.24-bookworm AS build

ARG TAGLIB_VERSION=2.1.1
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates cmake curl g++ libz-dev make \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL "https://github.com/taglib/taglib/archive/refs/tags/v${TAGLIB_VERSION}.tar.gz" -o /tmp/taglib.tar.gz \
    && mkdir /tmp/taglib \
    && tar -xzf /tmp/taglib.tar.gz -C /tmp/taglib --strip-components=1 \
    && cmake -S /tmp/taglib -B /tmp/taglib/build \
        -DBUILD_SHARED_LIBS=ON \
        -DBUILD_TESTING=OFF \
        -DCMAKE_BUILD_TYPE=Release \
        -DCMAKE_INSTALL_PREFIX=/usr/local \
    && cmake --build /tmp/taglib/build --parallel \
    && cmake --install /tmp/taglib/build \
    && ldconfig

WORKDIR /src
COPY go.mod go.sum ./
COPY vendor ./vendor
RUN go mod verify

COPY . .
RUN CGO_ENABLED=1 go test ./cmd/beatsweb ./internal/beatport ./config \
    && CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/beatportdl ./cmd/beatportdl \
    && CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/beats-web ./cmd/beatsweb

FROM debian:bookworm-slim

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates curl ffmpeg libstdc++6 zlib1g \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /usr/local/lib/libtag.so* /usr/local/lib/
COPY --from=build /usr/local/lib/libtag_c.so* /usr/local/lib/
COPY --from=build /out/beatportdl /usr/local/bin/beatportdl
COPY --from=build /out/beats-web /usr/local/bin/beats-web
RUN ldconfig \
    && useradd --create-home --uid 10001 beats \
    && mkdir -p /data /downloads \
    && chown -R beats:beats /data /downloads

USER beats
WORKDIR /data
ENV BEATS_ADDR=0.0.0.0:8080 \
    BEATPORTDL_BIN=/usr/local/bin/beatportdl \
    BEATS_DATA_DIR=/data \
    BEATS_FOLDERS=Downloads=/downloads

VOLUME ["/data", "/downloads"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -fsS http://127.0.0.1:8080/api/folders || exit 1

ENTRYPOINT ["/usr/local/bin/beats-web"]
