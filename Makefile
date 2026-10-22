BINARY   := cert
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)
GOFLAGS  := -trimpath
INSTALL_DIR ?= /usr/local/bin

# os/arch pairs shipped in releases (keep in sync with install.sh)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: all build linux linux-arm release install test vet fmt clean help

# Build the binary for the current platform
build:
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) .

# Cross-compile Linux x86_64
linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY)-linux .

# Cross-compile Linux ARM64
linux-arm:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY)-linux-arm64 .

# Build every release artifact into dist/ (used by CI and install.sh)
release: clean
	@mkdir -p dist
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		out=dist/$(BINARY)-$$os-$$arch; \
		echo "building $$out ($(VERSION))"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $$out . || exit 1; \
	done
	@cd dist && (shasum -a 256 $(BINARY)-* > checksums.txt 2>/dev/null || sha256sum $(BINARY)-* > checksums.txt)
	@ls -l dist

# Install into INSTALL_DIR (defaults to /usr/local/bin)
install: build
	install -d $(INSTALL_DIR)
	install -m 755 $(BINARY) $(INSTALL_DIR)/$(BINARY)

# Run tests
test:
	go test ./...

# Static analysis
vet:
	go vet ./...

# Format source code
fmt:
	gofmt -w *.go

# Remove build artifacts
clean:
	rm -f $(BINARY) $(BINARY)-linux $(BINARY)-linux-arm64
	rm -rf dist

help:
	@echo "Targets:"
	@echo "  make build       Build for the current platform"
	@echo "  make linux       Cross-compile Linux x86_64"
	@echo "  make linux-arm   Cross-compile Linux ARM64"
	@echo "  make release     Build all release artifacts into dist/"
	@echo "  make install     Install to $(INSTALL_DIR)"
	@echo "  make test        Run tests"
	@echo "  make vet         Run go vet"
	@echo "  make fmt         Format source code"
	@echo "  make clean       Remove build artifacts"
