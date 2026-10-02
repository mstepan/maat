.PHONY: build run test fmt vet clean

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

.PHONY: docker-build compose-up integration
MAAT_DOCKER_ARCH ?= $(shell docker version --format '{{.Server.Arch}}')

docker-build:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=$(MAAT_DOCKER_ARCH) go build -o bin/maat-linux .
	docker compose build

compose-up: docker-build
	sh deploy/prepare.sh
	docker compose up -d

integration:
	python3 deploy/integration.py

.PHONY: fault-build

# Test-only binary: normal builds contain no crash-injection filesystem hooks.
fault-build:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=$(MAAT_DOCKER_ARCH) go build -tags maat_faults -o bin/maat-faults-linux .
