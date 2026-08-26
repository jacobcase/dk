BINARY  := dk
PKG     := ./cmd/dk
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/jacobcase/dk/internal/cli.Version=$(VERSION)

.PHONY: all build install test test-race cover lint fmt tidy clean

all: lint test build

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) $(PKG)

install:
	go install -trimpath -ldflags '$(LDFLAGS)' $(PKG)

test:
	go test ./...

test-race:
	go test -race -cover ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

# gofmt -l prints files needing formatting; fail if the list is non-empty.
lint:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed:"; echo "$$unformatted"; exit 1; \
	fi
	go vet ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

clean:
	rm -rf bin coverage.out
