GO ?= go
VERSION ?= 0.1.0
PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin
BINARY ?= jesusftp
LDFLAGS ?= -s -w -X github.com/jesusftp/jesusftp/internal/cli.Version=$(VERSION)

.PHONY: build install update test
build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "$(BINARY)" .

install:
	mkdir -p "$(BINDIR)"
	@set -eu; \
	tmp=$$(mktemp -d "$(BINDIR)/.jesusftp-install.XXXXXX"); \
	trap 'rm -rf "$$tmp"' EXIT; \
	trap 'exit 130' INT; \
	trap 'exit 143' TERM; \
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "$$tmp/$(BINARY)" .; \
	chmod 0755 "$$tmp/$(BINARY)"; \
	mv -fT -- "$$tmp/$(BINARY)" "$(BINDIR)/$(BINARY)"; \
	printf 'Installed %s\n' "$(BINDIR)/$(BINARY)"

update: install

test:
	$(GO) test -race ./...
