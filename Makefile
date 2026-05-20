# clutch / tq CLI build matrix.
#
# Default target builds for the host into bin/clutch. Pass BRAND=tq to
# produce the TeleQuick-branded binary (bin/tq) — the brand name is
# stamped into the binary at link time so usage text, the dashboard
# title and the version line all show the right name. `make release`
# builds the full cross-platform matrix into bin/<os>-<arch>/<brand>[.exe].
# Pure Go, no CGO — these binaries are static and copy-deployable.

# BRAND selects which name the produced binary answers to. Only `clutch`
# and `tq` are supported here — if you need a third brand, add it to
# VALID_BRANDS so a typo doesn't silently produce an unknown-brand build.
BRAND      ?= clutch
VALID_BRANDS := clutch tq
ifeq ($(filter $(BRAND),$(VALID_BRANDS)),)
$(error BRAND must be one of: $(VALID_BRANDS) (got "$(BRAND)"))
endif

PKG        := ./cmd/clutch
BIN_DIR    := bin
BIN_NAME   := $(BRAND)
VERSION    ?= $(shell git -C $(CURDIR) describe --tags --always --dirty 2>/dev/null || echo dev)

# -s -w strips DWARF + symbol tables; ~30% smaller binaries with no
# runtime cost. -trimpath removes local filesystem paths from the binary
# (reproducible builds + smaller diff against an upstream).
# -X main.brand stamps the brand name into the binary so all user-facing
# strings switch from "clutch" to whatever BRAND was set to.
GOFLAGS    := -trimpath
LDFLAGS    := -s -w -X main.brand=$(BRAND)
GO         := go

# CGO off everywhere: we're pure Go (quic-go + x/sys), and disabling cgo
# means cross-compilation Just Works without a C toolchain per target.
export CGO_ENABLED := 0

# os/arch → bin/<os>-<arch>/clutch[.exe]
TARGETS := \
	linux/amd64 \
	linux/arm64 \
	windows/amd64 \
	darwin/arm64

.PHONY: all
all: build

.PHONY: build
build: ## Build for the host (override binary name with BRAND=tq)
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BIN_NAME) $(PKG)
	@echo "  → $(BIN_DIR)/$(BIN_NAME)"

.PHONY: clutch tq
clutch: ## Host build of the ClutchCall-branded binary
	$(MAKE) build BRAND=clutch

tq: ## Host build of the TeleQuick-branded binary
	$(MAKE) build BRAND=tq

.PHONY: release
release: $(addprefix release-,$(subst /,-,$(TARGETS))) ## Build the full cross-platform matrix
	@echo
	@ls -la $(BIN_DIR)/*/$(BIN_NAME)* 2>/dev/null || true

# Pattern rule: `make release-linux-amd64`, etc. The substitution turns
# the dash-form (linux-amd64) back into GOOS / GOARCH. Per-platform
# directories are namespaced by BRAND so `make release` and
# `make release BRAND=tq` produce side-by-side artifacts in bin/.
.PHONY: release-%
release-%:
	@os=$(word 1,$(subst -, ,$*)); \
	 arch=$(word 2,$(subst -, ,$*)); \
	 outdir=$(BIN_DIR)/$(BIN_NAME)-$$os-$$arch; \
	 ext=""; \
	 [ "$$os" = "windows" ] && ext=".exe"; \
	 mkdir -p $$outdir; \
	 echo "  → $$outdir/$(BIN_NAME)$$ext"; \
	 GOOS=$$os GOARCH=$$arch $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" \
	   -o $$outdir/$(BIN_NAME)$$ext $(PKG)

.PHONY: linux
linux: release-linux-amd64 release-linux-arm64 ## Both Linux variants

.PHONY: windows
windows: release-windows-amd64 ## Windows amd64

.PHONY: darwin
darwin: release-darwin-arm64 ## macOS Apple Silicon

# `make dist` packages each target into a tarball / zip ready to upload.
# Linux + macOS go in tar.gz, Windows in zip (the platform convention).
# Filenames are namespaced by BRAND so you can dist clutch and tq side
# by side without collisions.
.PHONY: dist
dist: release
	@cd $(BIN_DIR) && for d in */; do \
	   name=$${d%/}; \
	   case $$name in $(BIN_NAME)-*) ;; *) continue ;; esac; \
	   plat=$${name#$(BIN_NAME)-}; \
	   if echo $$plat | grep -q windows; then \
	     zip -qj $(BIN_NAME)-$(VERSION)-$$plat.zip $$d/$(BIN_NAME).exe; \
	     echo "  → $(BIN_DIR)/$(BIN_NAME)-$(VERSION)-$$plat.zip"; \
	   else \
	     tar -C $$d -czf $(BIN_NAME)-$(VERSION)-$$plat.tar.gz $(BIN_NAME); \
	     echo "  → $(BIN_DIR)/$(BIN_NAME)-$(VERSION)-$$plat.tar.gz"; \
	   fi; \
	 done

.PHONY: test
test: ## Run unit tests
	$(GO) test ./...

.PHONY: vet
vet: ## go vet
	$(GO) vet ./...

.PHONY: tidy
tidy: ## go mod tidy
	$(GO) mod tidy

.PHONY: clean
clean: ## Remove built binaries
	rm -rf $(BIN_DIR)

.PHONY: help
help: ## Show this help
	@awk 'BEGIN{FS=":.*##"; printf "Targets:\n"} /^[a-zA-Z_%-]+:.*##/ {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
