# claude-ping — build a single self-contained Go binary (no external deps).
#
#   make            build ./claude-ping for this machine (drives SSH from your laptop)
#   make linux      cross-compile dist/claude-ping-linux-amd64 (deploy for `heartbeat`)
#   make linux-arm  cross-compile dist/claude-ping-linux-arm64
#   make install    install to $GOBIN (or ~/go/bin)
#   make fmt vet    format / vet

BIN := claude-ping
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build linux linux-arm install fmt vet clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) .

linux:
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BIN)-linux-amd64 .

linux-arm:
	GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BIN)-linux-arm64 .

install:
	go install -ldflags "$(LDFLAGS)" .

fmt:
	gofmt -w .

vet:
	go vet ./...

clean:
	rm -f $(BIN)
	rm -rf dist
