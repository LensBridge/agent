VERSION ?= 0.1.0-dev
PKG     := github.com/utmmsa/musallahboard-agent
LDFLAGS := -s -w -X $(PKG)/internal/version.Version=$(VERSION)

.PHONY: build build-arm64 build-amd64 tidy test clean \
        deploy install-remote install-remote-x86 package package-amd64

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o build/musallahboard-agent ./cmd/agent

build-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
		go build -trimpath -ldflags "$(LDFLAGS)" -o build/musallahboard-agent-arm64 ./cmd/agent

build-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -trimpath -ldflags "$(LDFLAGS)" -o build/musallahboard-agent-amd64 ./cmd/agent

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
# containing the binary, install/uninstall/update scripts, the systemd unit,
# and the sudoers allow-list. On the Pi:
#   tar xzf musallahboard-agent-*.tar.gz
#   sudo bash musallahboard-agent-*/packaging/install.sh --token=X --backend=Y
package: build-arm64
	@rm -rf build/pkg && mkdir -p build/pkg/musallahboard-agent-$(VERSION)-arm64
	@cp build/musallahboard-agent-arm64 \
	    build/pkg/musallahboard-agent-$(VERSION)-arm64/musallahboard-agent
	@cp -r packaging build/pkg/musallahboard-agent-$(VERSION)-arm64/
	@sed -i 's/\r//' build/pkg/musallahboard-agent-$(VERSION)-arm64/packaging/*.sh
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
	@sed -i 's/\r//' build/pkg/musallahboard-agent-$(VERSION)-amd64/packaging/*.sh
	@chmod +x build/pkg/musallahboard-agent-$(VERSION)-amd64/packaging/*.sh
	@cp README.md build/pkg/musallahboard-agent-$(VERSION)-amd64/ 2>/dev/null || true
	@tar czf build/musallahboard-agent-$(VERSION)-amd64.tar.gz \
	    -C build/pkg musallahboard-agent-$(VERSION)-amd64
	@echo "Wrote build/musallahboard-agent-$(VERSION)-amd64.tar.gz"
