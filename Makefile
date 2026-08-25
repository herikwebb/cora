VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || printf dev)
SOURCE_SHA ?= $(shell git rev-parse HEAD 2>/dev/null || printf unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X github.com/herikwebb/cora/internal/cli.Version=$(VERSION) \
	-X github.com/herikwebb/cora/internal/cli.SourceSHA=$(SOURCE_SHA) \
	-X github.com/herikwebb/cora/internal/cli.BuildTime=$(BUILD_TIME)
INSTALL_DIR ?= $(HOME)/.local/bin

.PHONY: build install test clean

build:
	mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/cora ./cmd/cora

install:
	mkdir -p "$(INSTALL_DIR)"
	GOBIN="$(INSTALL_DIR)" go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/cora
	@printf 'Installed cora to %s\n' "$(INSTALL_DIR)/cora"
	@case ":$(PATH):" in \
		*":$(INSTALL_DIR):"*) ;; \
		*) printf 'Warning: %s is not on PATH.\n' "$(INSTALL_DIR)" ;; \
	esac

test:
	go test -race ./...
	go vet ./...

clean:
	go clean
	rm -f bin/cora
