BINARY := transcode-server

ifdef FFMPEG_PREFIX
PKG_CONFIG_PATH := $(FFMPEG_PREFIX)/lib/pkgconfig$(if $(PKG_CONFIG_PATH),:$(PKG_CONFIG_PATH))
CGO_LDFLAGS := -Wl,--disable-new-dtags -Wl,-rpath,$(FFMPEG_PREFIX)/lib $(CGO_LDFLAGS)
export PKG_CONFIG_PATH
export CGO_LDFLAGS
else ifneq ($(shell pkg-config --exists libavformat 2>/dev/null && echo yes),yes)
$(error FFmpeg not found via pkg-config. Install FFmpeg dev packages (e.g. libavformat-dev) or set FFMPEG_PREFIX to a FFmpeg shared build)
endif

.PHONY: build test test-race clean vet

build: $(BINARY)

$(BINARY):
	go build -o $@ ./cmd/transcode-server

test:
	go test ./... -count=1

test-race:
	go test -race ./... -count=1

clean:
	rm -f $(BINARY)
	rm -rf build/

vet:
	go vet ./...
