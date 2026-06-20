# Hina V2 — control-plane build.
# The control plane is CGo-free on purpose (keeps native Windows builds
# compiler-free). Local ONNX adapters arrive later behind a build tag.

BIN      := bin/hina
PKG      := ./...
GO       ?= go
GOFLAGS  :=
export CGO_ENABLED = 0

.PHONY: all build test vet tidy run doctor cross clean gen-ts build-onnx vet-onnx test-onnx

all: tidy vet test build

build:
	$(GO) build $(GOFLAGS) -o $(BIN) ./cmd/hina

test:
	$(GO) test $(GOFLAGS) $(PKG)

vet:
	$(GO) vet $(PKG)

# Local-inference build (Phase 4): links ONNX Runtime via the yalue binding behind
# the `onnx` build tag. Needs a C compiler (CGO_ENABLED=1) but NOT the ORT library
# at build time (it is dlopen'd at runtime). Tests that actually run a model skip
# unless an ORT 1.26.0 shared library is provided via ONNXRUNTIME_SHARED_LIBRARY_PATH.
build-onnx:
	CGO_ENABLED=1 $(GO) build -tags onnx $(GOFLAGS) -o $(BIN) ./cmd/hina

vet-onnx:
	CGO_ENABLED=1 $(GO) vet -tags onnx $(PKG)

test-onnx:
	CGO_ENABLED=1 $(GO) test -tags onnx $(PKG)

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

# Regenerate the TypeScript wire types from the Go DTOs (internal/wire, events).
gen-ts:
	$(GO) run github.com/gzuidhof/tygo@latest generate

clean:
	rm -rf bin
