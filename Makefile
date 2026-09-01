# Makefile for cpa-plugin-reset-priority.
#
# The plugin is a CGO c-shared library, so `build` targets the native platform
# of the machine running make. Release Linux libraries are built on matching-
# architecture runners inside pinned manylinux2014 containers; macOS and
# Windows release libraries are built natively on their hosted runners.
#
# On Linux, `make build` produces reset-priority.so in the repository root.

GO       ?= go
GOFMT    ?= gofmt
READELF  ?= readelf
ID       := reset-priority
DIST     ?= dist
GLIBC_MAX ?= 2.17

# GLIBC_ENFORCE=0 makes `make package` skip the strict Linux GLIBC gate so
# ordinary CI on a modern distribution (e.g. Ubuntu 24.04, GLIBC 2.39) can
# still exercise packaging and archive verification. The default stays strict,
# so the pinned manylinux2014 release jobs keep enforcing GLIBC <= $(GLIBC_MAX)
# without passing anything extra.
GLIBC_ENFORCE ?= 1

# VERSION is derived from the compiled-in plugin version so the Makefile,
# packaging tool, and release metadata cannot drift apart.
VERSION ?= $(shell sed -n 's/^.*PluginVersion = "\([^"]*\)".*$$/\1/p' internal/plugin/runtime.go)

GOOS   ?= $(shell $(GO) env GOOS)
GOARCH ?= $(shell $(GO) env GOARCH)

ifeq ($(GOOS),windows)
LIB_EXT := .dll
else ifeq ($(GOOS),darwin)
LIB_EXT := .dylib
else
LIB_EXT := .so
endif
LIB := $(ID)$(LIB_EXT)

# Static analysis. STATICCHECK_VERSION pins the honnef.co/go/tools module
# version (v0.8.1 is staticcheck 2026.2.1) so lint results are reproducible.
# Override STATICCHECK to use a locally installed binary,
# e.g. STATICCHECK=staticcheck.
STATICCHECK_VERSION ?= v0.8.1
STATICCHECK ?= $(GO) run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)

.PHONY: all fmt fmt-check vet lint test race build check-linux-glibc package check-release clean

all: fmt-check vet test build

## fmt: rewrite all Go sources with gofmt
fmt:
	$(GOFMT) -w .

## fmt-check: fail if any Go source is not gofmt-formatted
fmt-check:
	@files="$$($(GOFMT) -l .)"; \
	if [ -n "$$files" ]; then \
		echo "gofmt required for:"; echo "$$files"; exit 1; \
	fi
	@echo "gofmt: OK"

## vet: run go vet on all packages
vet:
	$(GO) vet ./...

## lint: run staticcheck static analysis
lint:
	$(STATICCHECK) ./...

## test: run all unit tests (includes registry.json and packaging validation)
test:
	$(GO) test ./...

## race: run all unit tests under the race detector (requires CGO)
race:
	CGO_ENABLED=1 $(GO) test -race ./...

## build: build the native CPA shared library (reset-priority.so on Linux)
build:
	CGO_ENABLED=1 $(GO) build -buildmode=c-shared -o $(LIB) .

## check-linux-glibc: verify Linux ELF architecture and GLIBC <= $(GLIBC_MAX)
check-linux-glibc: build
	@if [ "$(GOOS)" != "linux" ]; then \
		echo "check-linux-glibc requires GOOS=linux, got $(GOOS)" >&2; exit 2; \
	fi
	@command -v "$(READELF)" >/dev/null 2>&1 || { \
		echo "readelf is required for the Linux compatibility gate" >&2; exit 2; \
	}
	@set -eu; \
	case "$(GOARCH)" in \
		amd64) expected_machine='Advanced Micro Devices X86-64' ;; \
		arm64) expected_machine='AArch64' ;; \
		*) echo "unsupported Linux release architecture: $(GOARCH)" >&2; exit 2 ;; \
	esac; \
	machine="$$($(READELF) -h "$(LIB)" | grep -m1 'Machine:' || true)"; \
	case "$$machine" in \
		*"$$expected_machine"*) ;; \
		*) echo "$(LIB) has the wrong ELF machine for $(GOARCH): $$machine" >&2; exit 1 ;; \
	esac; \
	versions="$$($(READELF) --version-info --wide "$(LIB)" | grep -oE 'GLIBC_[0-9]+(\.[0-9]+)+' | sort -Vu || true)"; \
	if [ -z "$$versions" ]; then \
		echo "$(LIB) exposes no readable GLIBC symbol-version requirements" >&2; exit 1; \
	fi; \
	max_major='$(GLIBC_MAX)'; max_major="$${max_major%%.*}"; \
	max_minor='$(GLIBC_MAX)'; max_minor="$${max_minor#*.}"; max_minor="$${max_minor%%.*}"; \
	bad=''; \
	for symbol in $$versions; do \
		version="$${symbol#GLIBC_}"; \
		major="$${version%%.*}"; \
		minor="$${version#*.}"; minor="$${minor%%.*}"; \
		if [ "$$major" -gt "$$max_major" ] || { [ "$$major" -eq "$$max_major" ] && [ "$$minor" -gt "$$max_minor" ]; }; then \
			bad="$$bad $$symbol"; \
		fi; \
	done; \
	if [ -n "$$bad" ]; then \
		echo "$(LIB) requires GLIBC newer than $(GLIBC_MAX):$$bad" >&2; exit 1; \
	fi; \
	printf 'Linux compatibility gate: %s, GLIBC requirements <= %s (%s)\n' \
		"$$expected_machine" '$(GLIBC_MAX)' "$$(printf '%s' "$$versions" | tr '\n' ' ')"

## package: build and produce dist/reset-priority_$(VERSION)_$(GOOS)_$(GOARCH).zip + .sha256
ifeq ($(GOOS)_$(GLIBC_ENFORCE),linux_1)
package: check-linux-glibc
else
package: build
endif
package:
	$(GO) run ./tools/packager -lib $(LIB) -version $(VERSION) -goos $(GOOS) -goarch $(GOARCH) -out $(DIST)

## check-release: validate archive layout and checksums for the native platform in dist/
check-release:
	$(GO) run ./tools/packager -verify -version $(VERSION) -out $(DIST) -require $(GOOS)_$(GOARCH)

## clean: remove build and packaging outputs
clean:
	rm -rf $(DIST) $(ID).so $(ID).dylib $(ID).dll $(ID).h
