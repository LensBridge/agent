VERSION ?= 0.1.0
# The GitHub release tag the packages are published under. Usually VERSION;
# CI sets it separately when the tag has a prefix.
RELEASE_TAG ?= $(VERSION)
# Release public keys compiled into the agent (docs/architecture.md 5):
# comma-separated base64, from `mbpack keygen`. Empty is a dev build, which
# accepts only release keys listed in the board's trust.json. CI passes the
# repository variable MB_RELEASE_PUBLIC_KEYS.
RELEASE_KEYS ?=
# Must match the module path in go.mod exactly, case included: the linker
# silently ignores -X for a symbol it cannot resolve, so a wrong-case path
# here ships a binary that reports "dev" forever (or trusts no release key).
PKG     := github.com/LensBridge/agent
LDFLAGS := -s -w -X $(PKG)/internal/version.Version=$(VERSION) \
           -X $(PKG)/internal/trust.BuiltinReleaseKeys=$(RELEASE_KEYS)
# Where the release channel points boards at (docs/architecture.md 9.4).
RELEASE_DOWNLOAD := https://github.com/LensBridge/agent/releases/download/$(RELEASE_TAG)

.PHONY: build build-arm64 build-amd64 tidy test clean \
        mbpush mbpush-windows mbpush-macos mbpush-linux \
        mbpack package-mbu package-mbu-amd64

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o build/musallahboard-agent ./cmd/agent

build-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
		go build -trimpath -ldflags "$(LDFLAGS)" -o build/musallahboard-agent-arm64 ./cmd/agent

build-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -trimpath -ldflags "$(LDFLAGS)" -o build/musallahboard-agent-amd64 ./cmd/agent

# mbpush: the laptop tool for the service port (docs/architecture.md 9.5).
# Pure Go (it only shells out to the system ssh for --ssh), so a plain
# cross-compile is enough.
mbpush: mbpush-windows mbpush-macos mbpush-linux

mbpush-windows:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
		go build -trimpath -ldflags "$(LDFLAGS)" -o build/mbpush-windows-amd64.exe ./cmd/mbpush

mbpush-macos:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 \
		go build -trimpath -ldflags "$(LDFLAGS)" -o build/mbpush-darwin-arm64 ./cmd/mbpush

mbpush-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -trimpath -ldflags "$(LDFLAGS)" -o build/mbpush-linux-amd64 ./cmd/mbpush

# mbpack: builds, signs, verifies and inspects .mbu packages (section 4.4).
# Host build: it runs where the packages are made, not on a board. `verify`
# also trusts the RELEASE_KEYS compiled in here.
mbpack:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o build/mbpack ./cmd/mbpack

# Signed agent packages plus the channel file boards poll (sections 4 and
# 9.4). The signing seed comes from the environment, never the command line:
#   MB_RELEASE_SIGNING_KEY=<base64 seed> make package-mbu VERSION=0.3.0 RELEASE_KEYS=<b64>
# $(1) is the architecture.
define agent_mbu
	@test -n "$$MB_RELEASE_SIGNING_KEY" || { echo "MB_RELEASE_SIGNING_KEY is not set (base64 Ed25519 seed from mbpack keygen)"; exit 1; }
	build/mbpack agent --version $(VERSION) --arch $(1) --key-env MB_RELEASE_SIGNING_KEY \
	    build/musallahboard-agent-$(1) -o build/musallahboard-agent-$(VERSION)-$(1).mbu
	@f=build/musallahboard-agent-$(VERSION)-$(1).mbu; \
	 sha=$$(sha256sum "$$f" | cut -d' ' -f1); \
	 bytes=$$(wc -c < "$$f" | tr -d ' '); \
	 printf '{"version":"%s","url":"%s","sha256":"%s","bytes":%s}\n' \
	     "$(VERSION)" "$(RELEASE_DOWNLOAD)/musallahboard-agent-$(VERSION)-$(1).mbu" "$$sha" "$$bytes" \
	     > build/agent-channel-$(1).json; \
	 echo "Wrote $$f and build/agent-channel-$(1).json"
endef

package-mbu: build-arm64 mbpack
	$(call agent_mbu,arm64)

package-mbu-amd64: build-amd64 mbpack
	$(call agent_mbu,amd64)

tidy:
	go mod tidy

test:
	go test ./...

clean:
	rm -rf build/
