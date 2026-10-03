VERSION ?= $(shell cat VERSION)
LDFLAGS := -s -w -X main.version=$(VERSION)
ARCHES := amd64 arm64

.PHONY: all build test proto dist licenses clean

all: test build

build:
	@for arch in $(ARCHES); do for cmd in vps-probe-agent vps-probe-server vps-probe-echo; do \
		echo "build $$cmd linux/$$arch"; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o dist/$$cmd-linux-$$arch ./cmd/$$cmd || exit 1; \
	done; done

# Release tarballs: dist/vps-probe-$(VERSION)-linux-<arch>.tar.gz, each with
# both binaries, install.sh, systemd units, example configs and the license
# files.
dist: build licenses
	@for arch in $(ARCHES); do \
		d=dist/vps-probe-$(VERSION)-linux-$$arch; rm -rf $$d; \
		mkdir -p $$d/bin $$d/systemd $$d/examples; \
		cp dist/vps-probe-agent-linux-$$arch $$d/bin/vps-probe-agent; \
		cp dist/vps-probe-server-linux-$$arch $$d/bin/vps-probe-server; \
		cp dist/vps-probe-echo-linux-$$arch $$d/bin/vps-probe-echo; \
		cp deploy/install.sh $$d/; chmod 0755 $$d/install.sh; \
		cp deploy/*.service $$d/systemd/; \
		cp deploy/agent.example.yml deploy/server.example.yml deploy/echo.example.yml deploy/cloudflared.example.yml $$d/examples/; \
		cp README.md LICENSE NOTICE THIRD_PARTY_LICENSES $$d/; \
		(cd $$d && sha256sum bin/* install.sh systemd/* examples/* README.md LICENSE NOTICE THIRD_PARTY_LICENSES > SHA256SUMS); \
		tar -C dist -czf $$d.tar.gz --owner=0 --group=0 vps-probe-$(VERSION)-linux-$$arch; \
		rm -rf $$d; \
		echo "dist: $$d.tar.gz"; \
	done
	@cd dist && sha256sum vps-probe-$(VERSION)-linux-*.tar.gz > vps-probe-$(VERSION).sha256

# THIRD_PARTY_LICENSES: license texts of the linked Go modules and the
# vendored web libraries. Commit it after dependency changes.
licenses:
	ARCHES="$(ARCHES)" ./scripts/third_party_licenses.sh

test:
	go vet ./...
	go test ./...

# Requires protoc and protoc-gen-go on PATH. Generated code is committed, so
# this is only needed after editing proto/.
proto:
	protoc --go_out=. --go_opt=module=vpsprobe proto/probe/v1/probe.proto

clean:
	rm -rf dist
