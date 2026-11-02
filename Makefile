VERSION ?= 0.1.0
BINARY := bin/gateway
GO_FLAGS := -tags embed -trimpath

.PHONY: all web build run dev test test-go test-web vet fmt clean docker

all: build

## web: install dependencies and build the React console into internal/server/dist
web:
	cd web && pnpm install --frozen-lockfile && pnpm build

## build: produce the single binary with the console embedded
build: web
	mkdir -p bin
	go build $(GO_FLAGS) -ldflags "-s -w -X main.version=$(VERSION)" -o $(BINARY) ./cmd/server

## run: build and start the gateway
run: build
	./$(BINARY)

## dev: run the Vite dev server (proxies /api and /v1 to :8080)
dev:
	cd web && pnpm dev

test: test-go test-web

test-go:
	go test ./...

test-web:
	cd web && pnpm typecheck

vet:
	go vet ./...

fmt:
	gofmt -w cmd internal

clean:
	rm -rf bin internal/server/dist

docker:
	docker build --build-arg VERSION=$(VERSION) -t model-relay:$(VERSION) .
