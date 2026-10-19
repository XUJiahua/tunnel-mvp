GO ?= go
.PHONY: build frontend test clean
frontend:
	cd web && npm ci --no-audit --no-fund && npm run build
	node scripts/embed.mjs
build: frontend
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/tunnel ./cmd/tunnel
test:
	$(GO) test -race ./...
	$(GO) vet ./...
	cd web && npm run typecheck
clean:
	rm -rf bin web/.next web/out internal/web/dist
	mkdir -p internal/web/dist
	touch internal/web/dist/.gitkeep
