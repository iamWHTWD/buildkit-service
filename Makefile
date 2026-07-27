BATCH_BINARY  := ./bin/buildctl-batch
BATCH_PKG     := ./cmd/buildctl-batch
DAEMON_BINARY := ./bin/buildctl-daemon
DAEMON_PKG    := ./cmd/buildctl-daemon
BINARIES      := $(BATCH_BINARY) $(DAEMON_BINARY)
GO            ?= go
GOARCH        ?= $(shell $(GO) env GOARCH)
GOOS          ?= linux

.PHONY: all clean test buildctl-batch buildctl-daemon

all: buildctl-batch buildctl-daemon

buildctl-batch:
	mkdir -p $(dir $(BATCH_BINARY))
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) \
		$(GO) build -o $(BATCH_BINARY) $(BATCH_PKG)

buildctl-daemon:
	mkdir -p $(dir $(DAEMON_BINARY))
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) \
		$(GO) build -o $(DAEMON_BINARY) $(DAEMON_PKG)

test:
	$(GO) test ./...

clean:
	rm -f $(BINARIES)
