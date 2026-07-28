.PHONY: build install test check

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PREFIX ?= /usr/local

build:
	go build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/joyrun ./cmd/joyrun

install: build
	install -d "$(DESTDIR)$(PREFIX)/bin"
	install -m 0755 bin/joyrun "$(DESTDIR)$(PREFIX)/bin/joyrun"

test:
	go test ./...

check:
	go test ./...
	go vet ./...
