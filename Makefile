BINARY := argocd-mcp
PKG := ./cmd/argocd-mcp

.PHONY: build test lint run-mcp run-http docker

build:
	go build -o bin/$(BINARY) $(PKG)

test:
	go test -race ./...

lint:
	golangci-lint run

run-mcp: build
	./bin/$(BINARY) -mcp

run-http: build
	./bin/$(BINARY) -http-addr=:8080

docker:
	TAG=dev SHA=$$(git rev-parse --short HEAD) \
	  docker buildx bake -f build/package/docker-bake.json
