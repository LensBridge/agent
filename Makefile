VERSION ?= 0.1.0-dev
PKG     := github.com/utmmsa/musallahboard-agent
LDFLAGS := -s -w -X $(PKG)/internal/version.Version=$(VERSION)

.PHONY: build build-arm64 tidy test clean install-local

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o build/musallahboard-agent ./cmd/agent

build-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
		go build -trimpath -ldflags "$(LDFLAGS)" -o build/musallahboard-agent-arm64 ./cmd/agent

tidy:
	go mod tidy

test:
	go test ./...

clean:
	rm -rf build/

# scp the arm64 binary to a Pi and restart the service in place.
# Usage: make install-local PI=admin@brothers-board.local
install-local: build-arm64
	@if [ -z "$(PI)" ]; then echo "Usage: make install-local PI=user@host"; exit 1; fi
	scp build/musallahboard-agent-arm64 $(PI):/tmp/musallahboard-agent
	ssh $(PI) 'sudo install -m 0755 /tmp/musallahboard-agent /usr/local/bin/musallahboard-agent && sudo systemctl restart musallahboard-agent.service'
