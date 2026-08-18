.PHONY: all build test test-race test-shortcuts tokens clean

all: test build

build:
	@mkdir -p bin
	go build -o bin/things-index ./cmd/things-index
	go build -o bin/things-index-server ./cmd/things-index-server
	go build -o bin/things-index-worker ./cmd/things-index-worker

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
