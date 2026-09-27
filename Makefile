GO_LDFLAGS := -s -w \
	-X github.com/jlbyh2o/ezdr/internal/version.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev) \
	-X github.com/jlbyh2o/ezdr/internal/version.Commit=$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown) \
	-X github.com/jlbyh2o/ezdr/internal/version.Date=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)

.PHONY: all build client portal web web-deps test lint fmt clean

all: build

## build: build the client and the portal (with the web UI embedded)
build: client portal

client:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(GO_LDFLAGS)' -o bin/ezdr ./cmd/ezdr

portal: web
	CGO_ENABLED=0 go build -trimpath -tags webui -ldflags '$(GO_LDFLAGS)' -o bin/ezdr-portal ./cmd/ezdr-portal

web-deps:
	cd web && pnpm install --frozen-lockfile

## web: build the web UI into web/dist
web: web-deps
	cd web && pnpm build

## test: run Go tests
test:
	go test -race ./...

## lint: run Go and web linters
lint:
	golangci-lint run
	cd web && pnpm lint

## fmt: format Go code
fmt:
	golangci-lint fmt

clean:
	rm -rf bin web/dist
