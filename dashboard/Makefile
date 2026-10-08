.PHONY: all fmt test lint build run clean

all: fmt lint build test

fmt:
	go fmt ./...

test:
	go test ./...
	go test -race ./...

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0 run ./...

build:
	mkdir -p bin
	go build -o bin/maat-dashboard .

ARGS ?=
run:
	go run . $(ARGS)

clean:
	rm -rf bin
