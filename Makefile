.PHONY: build test test-go test-web front

build: front
	go build -o wterm-web ./cmd/wterm-web

front:
	cd web && pnpm install && pnpm build

test: test-go

test-go:
	go test ./... -count=1
