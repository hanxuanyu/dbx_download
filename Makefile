# dbxdl - build and test targets.
#
#   make build     compile ./bin/dbxdl
#   make test      run the unit tests
#   make check     gofmt + vet + test
#   make install   install onto $PATH (see INSTALL_DIR below)
#
# Installing for use from any directory:
#
#   make install                       # -> $GOBIN, else $(go env GOPATH)/bin
#   make install GOBIN=$HOME/.local/bin
#   make build && cp bin/dbxdl ~/.local/bin/
#
# The destination must already be on $PATH; check with "which dbxdl".

BINARY  := dbxdl
BIN_DIR := bin
PKG     := dbxdl
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(PKG)/internal/cli.Version=$(VERSION)

# Keep the build cache inside the project so builds work in sandboxes that
# cannot write to the default GOCACHE location.
export GOCACHE ?= $(CURDIR)/.gocache

# Where "make install" puts the binary: an explicit GOBIN wins, otherwise the
# Go default of $(go env GOPATH)/bin. Note that some toolchain managers
# (mise, asdf) set GOBIN to the toolchain's own bin directory; pass
# GOBIN=$HOME/go/bin to keep the binary independent of the toolchain.
INSTALL_DIR := $(if $(GOBIN),$(GOBIN),$(shell go env GOPATH)/bin)

.PHONY: all build test check fmt vet tidy install install-dir clean run help

all: check build

build:
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) .
	@echo "built $(BIN_DIR)/$(BINARY) ($(VERSION))"

install:
	go install -trimpath -ldflags "$(LDFLAGS)" .
	@echo "installed $(INSTALL_DIR)/$(BINARY) ($(VERSION))"
	@case ":$$PATH:" in *":$(INSTALL_DIR):"*) \
		echo "  -> $(INSTALL_DIR) is on \$$PATH; run \"dbxdl version\" to verify." ;; \
	*) \
		echo "  !! $(INSTALL_DIR) is NOT on \$$PATH."; \
		echo "     Add this to ~/.zshrc, then restart the shell:"; \
		echo "       export PATH=\"$(INSTALL_DIR):\$$PATH\"" ;; \
	esac

# Print the resolved install destination without installing anything.
install-dir:
	@echo $(INSTALL_DIR)

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
