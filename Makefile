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
        deploy install-remote install-remote-x86 package package-amd64 \
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

# Replace the binary on a remote host and restart the service (preserves enrollment).
# Usage: make deploy PI=admin@brothers-board.local
deploy: build-arm64
	@if [ -z "$(PI)" ]; then echo "Usage: make deploy PI=user@host"; exit 1; fi
	scp build/musallahboard-agent-arm64 packaging/update.sh $(PI):/tmp/
	ssh -t $(PI) 'sudo bash /tmp/update.sh /tmp/musallahboard-agent-arm64'

# First-time full install on a remote host via the release tarball.
# Usage: make install-remote PI=admin@pi.local TOKEN=xxx BACKEND=http://host:8080
install-remote: package
	@if [ -z "$(PI)" ]; then echo "Usage: make install-remote PI=user@host [TOKEN=xxx BACKEND=url]"; exit 1; fi
	scp build/musallahboard-agent-$(VERSION)-arm64.tar.gz $(PI):/tmp/
	ssh -t $(PI) 'cd /tmp && tar xzf musallahboard-agent-$(VERSION)-arm64.tar.gz && \
	    sudo bash musallahboard-agent-$(VERSION)-arm64/packaging/install.sh \
	    $(if $(TOKEN),--token=$(TOKEN)) $(if $(BACKEND),--backend=$(BACKEND))'

# First-time full install on a remote x86-64 host via the release tarball.
# Usage: make install-remote-x86 HOST=admin@server TOKEN=xxx BACKEND=http://host:8080
install-remote-x86: package-amd64
	@if [ -z "$(HOST)" ]; then echo "Usage: make install-remote-x86 HOST=user@host [TOKEN=xxx BACKEND=url]"; exit 1; fi
	scp build/musallahboard-agent-$(VERSION)-amd64.tar.gz $(HOST):/tmp/
	ssh -t $(HOST) 'cd /tmp && tar xzf musallahboard-agent-$(VERSION)-amd64.tar.gz && \
	    sudo bash musallahboard-agent-$(VERSION)-amd64/packaging/install.sh \
	    $(if $(TOKEN),--token=$(TOKEN)) $(if $(BACKEND),--backend=$(BACKEND))'

# Build a self-contained release tarball ready to ship to a Pi:
#   musallahboard-agent-$(VERSION)-arm64.tar.gz
# containing the binary, install/uninstall/update scripts, the systemd units
# (agent, kiosk, self-updater, USB import), the udev rules and the sudoers
# allow-list. On the Pi:
#   tar xzf musallahboard-agent-*.tar.gz
#   sudo bash musallahboard-agent-*/packaging/install.sh --token=X --backend=Y
package: build-arm64
	@rm -rf build/pkg && mkdir -p build/pkg/musallahboard-agent-$(VERSION)-arm64
	@cp build/musallahboard-agent-arm64 \
	    build/pkg/musallahboard-agent-$(VERSION)-arm64/musallahboard-agent
	@cp -r packaging build/pkg/musallahboard-agent-$(VERSION)-arm64/
	@sed -i 's/\r//' build/pkg/musallahboard-agent-$(VERSION)-arm64/packaging/*.sh \
	    build/pkg/musallahboard-agent-$(VERSION)-arm64/packaging/*.service \
	    build/pkg/musallahboard-agent-$(VERSION)-arm64/packaging/*.path \
	    build/pkg/musallahboard-agent-$(VERSION)-arm64/packaging/udev/*.rules
	@chmod +x build/pkg/musallahboard-agent-$(VERSION)-arm64/packaging/*.sh
# setup.sh is the whole point of the arm64 tarball: the one-line installer
# fetches this archive and runs it. Not shipped in the amd64 one — it is a
# Raspberry Pi wrapper and the x86 host is provisioned by the Packer image.
	@cp setup.sh build/pkg/musallahboard-agent-$(VERSION)-arm64/
	@sed -i 's/\r//' build/pkg/musallahboard-agent-$(VERSION)-arm64/setup.sh
	@chmod +x build/pkg/musallahboard-agent-$(VERSION)-arm64/setup.sh
# packaging/luks/ holds three extensionless scripts that run inside an
# initramfs, where a stray CR in a shebang produces no diagnosable error at all.
	@sed -i 's/\r//' build/pkg/musallahboard-agent-$(VERSION)-arm64/packaging/luks/*
	@chmod +x build/pkg/musallahboard-agent-$(VERSION)-arm64/packaging/luks/*
	@cp README.md build/pkg/musallahboard-agent-$(VERSION)-arm64/ 2>/dev/null || true
	@tar czf build/musallahboard-agent-$(VERSION)-arm64.tar.gz \
	    -C build/pkg musallahboard-agent-$(VERSION)-arm64
	@echo "Wrote build/musallahboard-agent-$(VERSION)-arm64.tar.gz"

package-amd64: build-amd64
	@rm -rf build/pkg && mkdir -p build/pkg/musallahboard-agent-$(VERSION)-amd64
	@cp build/musallahboard-agent-amd64 \
	    build/pkg/musallahboard-agent-$(VERSION)-amd64/musallahboard-agent
	@cp -r packaging build/pkg/musallahboard-agent-$(VERSION)-amd64/
	@sed -i 's/\r//' build/pkg/musallahboard-agent-$(VERSION)-amd64/packaging/*.sh \
	    build/pkg/musallahboard-agent-$(VERSION)-amd64/packaging/*.service \
	    build/pkg/musallahboard-agent-$(VERSION)-amd64/packaging/*.path \
	    build/pkg/musallahboard-agent-$(VERSION)-amd64/packaging/udev/*.rules
	@chmod +x build/pkg/musallahboard-agent-$(VERSION)-amd64/packaging/*.sh
	@cp README.md build/pkg/musallahboard-agent-$(VERSION)-amd64/ 2>/dev/null || true
	@tar czf build/musallahboard-agent-$(VERSION)-amd64.tar.gz \
	    -C build/pkg musallahboard-agent-$(VERSION)-amd64
	@echo "Wrote build/musallahboard-agent-$(VERSION)-amd64.tar.gz"
