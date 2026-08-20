.PHONY: all build dist-mac test test-race test-shortcuts tokens clean

all: test build

build:
	@mkdir -p bin
	go build -o bin/things-index ./cmd/things-index
	go build -o bin/things-index-server ./cmd/things-index-server
	go build -o bin/things-index-worker ./cmd/things-index-worker

# Universal macOS binary for the release asset the Mac one-liner downloads.
dist-mac:
	@mkdir -p dist
	CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w" -o dist/things-index-arm64 ./cmd/things-index
	CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 go build -ldflags="-s -w" -o dist/things-index-amd64 ./cmd/things-index
	lipo -create -output dist/things-index-darwin-universal dist/things-index-arm64 dist/things-index-amd64
	@rm dist/things-index-arm64 dist/things-index-amd64
	@ls -la dist/things-index-darwin-universal

test:
	go test ./...

test-race:
	go test -race ./...

test-shortcuts:
	THINGS_INDEX_UNSIGNED_SHORTCUT="$(PWD)/shortcuts/ThingsIndex Helper_unsigned.shortcut" \
	  go test ./shortcuts -run 'TestHelper|TestCompiled' -v

tokens:
	@echo "THINGS_INDEX_PUBLIC_TOKEN=$$(openssl rand -hex 32)"
	@echo "THINGS_INDEX_WORKER_TOKEN=$$(openssl rand -hex 32)"
	@echo "THINGS_INDEX_DASHBOARD_TOKEN=$$(openssl rand -hex 32)"

clean:
	rm -rf bin
