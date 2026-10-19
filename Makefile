GO ?= go
GORELEASER ?= goreleaser
.PHONY: build frontend test clean release-check release-snapshot
frontend:
	cd web && pnpm install --frozen-lockfile && pnpm run build
	node scripts/embed.mjs
build: frontend
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/tunnel ./cmd/tunnel
test:
	bash scripts/test-prepare-release.sh
	$(GO) test -race ./...
	TUNNEL_TEST_STORAGE=sqlite $(GO) test -race ./internal/control
	$(GO) vet ./...
	cd web && pnpm run typecheck
clean:
	rm -rf bin web/.next web/out internal/web/dist
	mkdir -p internal/web/dist
	touch internal/web/dist/.gitkeep

release-check:
	$(GORELEASER) check
release-snapshot:
	$(GORELEASER) release --snapshot --clean --parallelism 2
	python3 scripts/verify-release.py dist
