.PHONY: build test test-go test-web front

build: front
	go build -o wterm-web ./cmd/wterm-web

front:
	cd web && pnpm install && pnpm build

test: test-go

test-go:
	go test ./... -count=1

# No frontend yet. Fail loudly rather than succeed silently: a declared but
# empty test target reports success in CI while running nothing.
test-web:
	@echo "test-web: no frontend yet (see Phase D); wire up web/ tests here" >&2; exit 1
