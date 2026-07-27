.PHONY: build test check

build:
	go build -o bin/joyrun ./cmd/joyrun

test:
	go test ./...

check:
	go test ./...
	go vet ./...
