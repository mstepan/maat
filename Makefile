.PHONY: all build run test fmt lint clean

all: clean fmt lint build test integration

build:
	mkdir -p bin
	go build -o bin/maat .

run:
	go run .

test:
	go test ./...
	go test -race -tags maat_faults ./...
	python3 -m unittest discover -s deploy -p 'test_*.py'

fmt:
	go fmt ./...

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0 run ./...

clean:
	rm -rf bin

.PHONY: linux-build docker-build compose-up compose-down compose-clean integration
MAAT_DOCKER_ARCH ?= $(shell docker version --format '{{.Server.Arch}}')

linux-build:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=$(MAAT_DOCKER_ARCH) go build -o bin/maat-linux .

docker-build: linux-build
	docker compose build

compose-up: docker-build
	sh deploy/prepare.sh
	docker compose up -d --no-recreate

compose-down:
	docker compose stop

compose-clean:
	docker compose down --volumes --remove-orphans --rmi local
	rm -f .secrets/postgres-password .secrets/replication-password
	rmdir .secrets 2>/dev/null || true

integration: linux-build
	python3 deploy/run_integration.py

.PHONY: fault-build

# Test-only binary: normal builds contain no crash-injection filesystem hooks.
fault-build:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=$(MAAT_DOCKER_ARCH) go build -tags maat_faults -o bin/maat-faults-linux .
