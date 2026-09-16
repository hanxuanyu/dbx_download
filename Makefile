# dbxdl - build and test targets.
#
#   make build     compile ./bin/dbxdl
#   make test      run the unit tests
#   make check     gofmt + vet + test
#   make install   install into $GOBIN (or $GOPATH/bin)

BINARY  := dbxdl
BIN_DIR := bin
PKG     := dbxdl
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(PKG)/internal/cli.Version=$(VERSION)

# Keep the build cache inside the project so builds work in sandboxes that
# cannot write to the default GOCACHE location.
export GOCACHE ?= $(CURDIR)/.gocache

.PHONY: all build test check fmt vet tidy install clean run help

all: check build

build:
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) .
	@echo "built $(BIN_DIR)/$(BINARY) ($(VERSION))"

install:
	go install -trimpath -ldflags "$(LDFLAGS)" .
	@echo "installed $(BINARY)"

test:
	go test ./...

fmt:
	gofmt -l -w .

vet:
	go vet ./...

tidy:
	go mod tidy

check: fmt vet test

run: build
	./$(BIN_DIR)/$(BINARY) $(ARGS)

clean:
	rm -rf $(BIN_DIR) .gocache

help:
	@./$(BIN_DIR)/$(BINARY) help 2>/dev/null || go run . help
