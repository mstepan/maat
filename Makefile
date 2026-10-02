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
