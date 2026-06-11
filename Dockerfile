FROM golang:1.26-bookworm AS builder

ARG TARGETARCH

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    xz-utils \
    pkg-config \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /tmp

RUN case "$TARGETARCH" in \
      amd64) arch=linux64 ;; \
      arm64) arch=linuxarm64 ;; \
      *) echo "unsupported arch: $TARGETARCH" && exit 1 ;; \
    esac && \
    url="https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-n8.1-latest-${arch}-gpl-shared-8.1.tar.xz" && \
    echo "downloading FFmpeg from $url" && \
    curl -fsSL "$url" -o ffmpeg.tar.xz && \
    mkdir -p /tmp/ffmpeg && \
    tar -xJf ffmpeg.tar.xz --strip-components=1 -C /tmp/ffmpeg && \
    mkdir -p /ffmpeg-root/usr/lib /ffmpeg-root/usr/include && \
    cp -a /tmp/ffmpeg/lib/* /ffmpeg-root/usr/lib/ && \
    cp -a /tmp/ffmpeg/include/* /ffmpeg-root/usr/include/ && \
    cp -a /ffmpeg-root/usr/lib/* /usr/lib/ && \
    cp -a /ffmpeg-root/usr/include/* /usr/include/ && \
    rm -rf /tmp/ffmpeg ffmpeg.tar.xz

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=1

RUN pkg-config --libs libavcodec libavformat libavutil libswscale libswresample \
    && go build -trimpath -ldflags="-s -w" -o /out/transcode-server ./cmd/transcode-server

FROM gcr.io/distroless/cc-debian13

COPY --from=builder /out/transcode-server /transcode-server

COPY --from=builder /ffmpeg-root/usr/lib/ /usr/lib/

EXPOSE 8080

ENTRYPOINT ["/transcode-server"]
