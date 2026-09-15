BINARY=aida

# Codesigning: a stable Developer ID signature keeps the macOS Input
# Monitoring grant (push-to-talk) valid across rebuilds. An adhoc signature
# would not - its designated requirement is a raw cdhash that changes every
# build, silently killing the SayoDevice button with no error surfaced.
CODESIGN_ID ?= Developer ID Application: Ryan L'Italien (92Y3P4P8RZ)
CODESIGN_IDENTIFIER ?= com.ryanlitalien.aida
GOBIN_DIR := $(shell go env GOPATH)/bin

build:
	go build -o bin/$(BINARY) ./cmd/aida

# Cross-compile a linux/arm64 aida for provisioning into an sbx microVM (the
# `aida loop --sandbox docker` tier copies this in so a real `aida --agent` runs
# inside the Linux VM). Pure-Go sqlite + CGO_ENABLED=0 cross-compiles cleanly.
build-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/$(BINARY)-linux-arm64 ./cmd/aida

# Cross-compile every release target into bin/.
build-all:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o bin/$(BINARY)-darwin-arm64 ./cmd/aida
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -o bin/$(BINARY)-darwin-amd64 ./cmd/aida
	CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build -o bin/$(BINARY)-linux-arm64 ./cmd/aida
	CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -o bin/$(BINARY)-linux-amd64 ./cmd/aida

# One real binary at $(GOBIN_DIR)/aida, signed, with ~/bin/aida as a symlink
# to it. Previously this cp'd a second independent copy into ~/bin, which
# meant two binaries to keep straight and two separate TCC grants.
install:
	go install ./cmd/aida
	@$(MAKE) --no-print-directory sign
	@mkdir -p ~/bin && ln -sfn "$(GOBIN_DIR)/$(BINARY)" ~/bin/$(BINARY)
	@echo "✓ installed $(GOBIN_DIR)/$(BINARY) (~/bin/$(BINARY) -> symlink)"

# Sign the installed binary with a stable Developer ID identity so the macOS
# Input Monitoring grant for push-to-talk survives rebuilds. Falls back to a
# warning (not a failure) on machines without the cert, since an adhoc
# signature still runs fine - it just won't keep the TCC grant.
sign:
	@if [ "$$(uname -s)" != "Darwin" ]; then \
		echo "sign: not on Darwin, skipping codesign"; \
	elif security find-identity -v -p codesigning | grep -q "$(CODESIGN_ID)"; then \
		codesign --force --sign "$(CODESIGN_ID)" --identifier "$(CODESIGN_IDENTIFIER)" "$(GOBIN_DIR)/$(BINARY)"; \
		echo "✓ signed $(GOBIN_DIR)/$(BINARY) with $(CODESIGN_ID)"; \
	else \
		echo "WARNING: Developer ID identity \"$(CODESIGN_ID)\" not found; $(GOBIN_DIR)/$(BINARY) stays adhoc-signed."; \
		echo "         The Input Monitoring grant for push-to-talk will break on every rebuild."; \
	fi

test:
	go test ./...

# Run the golden query regression suite end-to-end against the current
# binary + prompts. Requires a working ANTHROPIC_API_KEY in the environment.
golden: build
	./bin/$(BINARY) golden run

fmt:
	go fmt ./...

vet:
	go vet ./...

# Regression scan for personal/employer data that must never reach the
# public tree (former employer name, real merchant examples, personal
# email domains, Tailscale IPs, 1Password ids, real hostnames, phone
# number). See scripts/oss-scan.sh for the full pattern list and the
# reasoning behind its allowlisted false positives.
oss-scan:
	./scripts/oss-scan.sh

clean:
	rm -rf bin/ dist/

release-local:
	goreleaser release --snapshot --clean

# One-shot setup for the Jarvis voice layer on a fresh macOS machine.
# Idempotent - safe to re-run any time. Installs ffmpeg + whisper-cpp
# via brew, ensures piper is available, and downloads the voice/STT
# models to ~/.aida/jarvis/models/ (none of which are git-tracked).
jarvis-deps:
	./scripts/jarvis-deps.sh

# Install/uninstall a per-user LaunchAgent that runs `aida serve` at every
# login. Logs to ~/.aida/jarvis/launchd.{out,err}.log. Run install once;
# launchd handles restart-on-crash and start-at-login from then on.
jarvis-install-launchd:
	./scripts/jarvis-install-launchd.sh install

jarvis-uninstall-launchd:
	./scripts/jarvis-install-launchd.sh uninstall

jarvis-status-launchd:
	./scripts/jarvis-install-launchd.sh status

.PHONY: build build-linux-arm64 build-all install sign test golden fmt vet oss-scan clean release-local \
        jarvis-deps jarvis-install-launchd jarvis-uninstall-launchd jarvis-status-launchd
