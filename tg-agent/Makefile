GO ?= go
.PHONY: build test check release
build:
	$(GO) build -trimpath -o tg-agent .
test:
	$(GO) test -race ./...
check:
	$(GO) vet ./...
	$(GO) test ./...
release:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags='-s -w' -o dist/tg-agent-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags='-s -w' -o dist/tg-agent-linux-arm64 .
