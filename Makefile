.PHONY: build test web dev
build: web
	go build -trimpath -o bin/vaultpilot ./cmd/vaultpilot
	go build -trimpath -o bin/vprescue ./cmd/vprescue
web:
	cd web && npm ci && npm run build
test:
	go test -race ./...
dev:
	go run ./cmd/vaultpilot
