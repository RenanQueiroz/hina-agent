# Hina V2 — control-plane build.
# The control plane is CGo-free on purpose (keeps native Windows builds
# compiler-free). Local ONNX adapters arrive later behind a build tag.

BIN      := bin/hina
PKG      := ./...
GO       ?= go
GOFLAGS  :=
export CGO_ENABLED = 0

.PHONY: all build test vet tidy run doctor cross clean

all: tidy vet test build

build:
	$(GO) build $(GOFLAGS) -o $(BIN) ./cmd/hina

test:
	$(GO) test $(GOFLAGS) $(PKG)

vet:
	$(GO) vet $(PKG)

tidy:
	$(GO) mod tidy

run: build
	$(BIN) server

doctor: build
	$(BIN) doctor

# Prove the cross-platform build claim locally (Windows + macOS targets).
cross:
	GOOS=windows GOARCH=amd64 $(GO) build -o /dev/null ./cmd/hina && echo "windows/amd64 OK"
	GOOS=darwin  GOARCH=arm64 $(GO) build -o /dev/null ./cmd/hina && echo "darwin/arm64 OK"
	GOOS=linux   GOARCH=amd64 $(GO) build -o /dev/null ./cmd/hina && echo "linux/amd64 OK"

clean:
	rm -rf bin
