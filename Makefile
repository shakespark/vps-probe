VERSION ?= $(shell cat VERSION)
LDFLAGS := -s -w -X main.version=$(VERSION)
ARCHES := amd64 arm64

# Reproducible releases (docs/DESIGN.md §13.2): the exact Go release from
# go.mod's toolchain line (downloaded if the local one differs), no VCS
# stamping, and tarballs with fixed order, owners, modes and mtime (the
# commit's time), so a rebuild of a tag matches the CI artifacts byte for byte.
export GOTOOLCHAIN := $(shell sed -n 's/^toolchain //p' go.mod)
SOURCE_DATE_EPOCH ?= $(shell git log -1 --format=%ct)
TAR := tar --sort=name --format=gnu --owner=0 --group=0 --numeric-owner \
	--mode=u=rwX,go=rX --mtime=@$(SOURCE_DATE_EPOCH)

.PHONY: all build test proto dist licenses release-verify release-sign demo demo-check clean

all: test build

build:
	@for arch in $(ARCHES); do for cmd in vps-probe-agent vps-probe-server vps-probe-echo; do \
		echo "build $$cmd linux/$$arch"; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" \
			-o dist/$$cmd-linux-$$arch ./cmd/$$cmd || exit 1; \
	done; done

# Release tarballs: dist/vps-probe-$(VERSION)-linux-<arch>.tar.gz, each with
# the three binaries, install.sh, systemd units, example configs and the license
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
		(cd $$d && LC_ALL=C sha256sum bin/* install.sh systemd/* examples/* README.md LICENSE NOTICE THIRD_PARTY_LICENSES > SHA256SUMS); \
		LC_ALL=C $(TAR) -C dist -cf $$d.tar vps-probe-$(VERSION)-linux-$$arch && gzip -9nf $$d.tar || exit 1; \
		rm -rf $$d; \
		echo "dist: $$d.tar.gz"; \
	done
	@cd dist && sha256sum vps-probe-$(VERSION)-linux-*.tar.gz > vps-probe-$(VERSION).sha256

# THIRD_PARTY_LICENSES: license texts of the linked Go modules and the
# vendored web libraries. Commit it after dependency changes.
licenses:
	ARCHES="$(ARCHES)" ./scripts/third_party_licenses.sh

# Check the draft GitHub release CI built for v$(VERSION) against a local
# rebuild of the tag; release-sign then signs it with the offline key and
# publishes it (docs/DESIGN.md §13.1).
release-verify:
	VERSION=$(VERSION) ./scripts/release.sh verify

release-sign:
	VERSION=$(VERSION) ./scripts/release.sh sign

# The static demo site (docs/DESIGN.md §14): the real UI plus web/demo/demo.js,
# which answers /api/* in the browser. Upload dist/demo/ to a static host,
# at the root of the site.
demo:
	rm -rf dist/demo
	mkdir -p dist/demo/static
	cp -r web/static/. dist/demo/static/
	mv dist/demo/static/index.html dist/demo/index.html
	cp web/demo/demo.js dist/demo/static/demo.js
	cp web/demo/_headers dist/demo/_headers
	sed -i 's|<script type="module" src="/static/app.js"></script>|<script src="/static/demo.js"></script>\n&|' dist/demo/index.html
	grep -q '/static/demo.js' dist/demo/index.html
	@echo "demo: dist/demo"

# demo.js must answer with the same fields as the real API. Needs node.
demo-check:
	node web/demo/check.js

test:
	go vet ./...
	go test ./...

# Requires protoc and protoc-gen-go on PATH. Generated code is committed, so
# this is only needed after editing proto/.
proto:
	protoc --go_out=. --go_opt=module=github.com/shakespark/vps-probe proto/probe/v1/probe.proto

clean:
	rm -rf dist
