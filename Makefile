.PHONY: dev-api dev-web build build-go build-web test test-web check clean

dev-api: build-web
	go run ./cmd/opsnerva serve

dev-web:
	pnpm --dir web run dev

build: build-go

build-web:
	pnpm --dir web install --frozen-lockfile
	pnpm --dir web run build

build-go: build-web
	mkdir -p bin
	go build -buildvcs=false -trimpath -ldflags="-s -w" -o bin/opsnerva ./cmd/opsnerva

test: build-web
	go test ./...

test-web: build-web

check: test build-go

clean:
	rm -rf bin
