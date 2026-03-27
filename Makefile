BINARY     := klimax
MODULE     := github.com/bcollard/klimax
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS    := -ldflags "-s -w -X main.version=$(VERSION)"
BUILD_DIR  := ./dist

# CGO_ENABLED=1 is required: Lima's instance/store packages link against macOS frameworks.
export CGO_ENABLED=1

ENTITLEMENTS := entitlements.plist

.PHONY: build sign test lint install dev-install clean tidy snapshot release-check

build:
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY) ./cmd/klimax/...
	codesign --sign - --entitlements $(ENTITLEMENTS) --force $(BUILD_DIR)/$(BINARY)

test:
	go test ./...

lint:
	golangci-lint run ./...

install:
	go install $(LDFLAGS) ./cmd/klimax/...

dev-install: build
	sudo cp $(BUILD_DIR)/$(BINARY) /usr/local/bin/$(BINARY)

clean:
	rm -rf $(BUILD_DIR)

tidy:
	go mod tidy

# Local dry-run release (no publish). Requires goreleaser.
snapshot:
	goreleaser release --snapshot --clean

# Validate .goreleaser.yaml
release-check:
	goreleaser check
