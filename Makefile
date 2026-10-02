.PHONY: build run test fmt vet clean all

all: clean fmt vet build test integration

build:
	mkdir -p bin
	go build -o bin/maat .

run:
	go run .

test:
	go test ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

clean:
	rm -f bin/maat

.PHONY: docker-build compose-up compose-down compose-clean integration
MAAT_DOCKER_ARCH ?= $(shell docker version --format '{{.Server.Arch}}')

docker-build:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=$(MAAT_DOCKER_ARCH) go build -o bin/maat-linux .
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

integration:
	python3 deploy/integration.py

.PHONY: fault-build

# Test-only binary: normal builds contain no crash-injection filesystem hooks.
fault-build:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=$(MAAT_DOCKER_ARCH) go build -tags maat_faults -o bin/maat-faults-linux .
