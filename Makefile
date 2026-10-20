GO ?= go
GORELEASER ?= goreleaser
GORELEASER_VERSION := 2.17.1
.PHONY: build frontend test clean release-check release-snapshot example-oidc
frontend:
	cd web && pnpm install --frozen-lockfile && pnpm run build
	node scripts/embed.mjs
build: frontend
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/tunnel ./cmd/tunnel
example-oidc:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) -C examples/oidc-app build -trimpath -o ../../bin/oidc-app .
test:
	$(GO) -C examples/oidc-app test -race ./...
	$(GO) -C third_party/remotedialer test -race ./...
	bash scripts/test-prepare-release.sh
	$(GO) test -race ./...
	TUNNEL_TEST_STORAGE=sqlite $(GO) test -race ./internal/control
	$(GO) -C examples/oidc-app vet ./...
	$(GO) -C third_party/remotedialer vet ./...
	$(GO) vet ./...
	cd web && pnpm run typecheck
clean:
	rm -rf bin web/.next web/out internal/web/dist
	mkdir -p internal/web/dist
	touch internal/web/dist/.gitkeep

release-check:
	@$(GORELEASER) --version | grep -Eq "GitVersion: +v?$(GORELEASER_VERSION)$$" || (echo "GoReleaser $(GORELEASER_VERSION) required" >&2; exit 1)
	$(GORELEASER) check
release-snapshot: release-check
	$(GORELEASER) release --snapshot --clean --parallelism 1
	python3 scripts/verify-release.py dist
