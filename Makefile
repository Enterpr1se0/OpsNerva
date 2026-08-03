.PHONY: dev-api dev-web build build-go build-web test test-web check clean

dev-api: build-web
	go run ./cmd/ops-agent serve

dev-web:
	npm --prefix web run dev

build: build-go

build-web:
	npm --prefix web install
	npm --prefix web run build

build-go: build-web
	mkdir -p bin
	go build -buildvcs=false -trimpath -ldflags="-s -w" -o bin/ops-agent ./cmd/ops-agent

test: build-web
	go test ./...

test-web: build-web

check: test build-go

clean:
	rm -rf bin
