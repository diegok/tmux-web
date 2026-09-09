.PHONY: build test test-go test-web front dist-keep

build: front
	go build -o wterm-web ./cmd/wterm-web

front:
	cd web && pnpm install && pnpm build
	$(MAKE) dist-keep

# internal/front/dist/.gitkeep is what lets `//go:embed all:dist` compile on a
# clone where the frontend has never been built -- an embed pattern that
# matches nothing is a compile error, and dist is build output that stays out
# of git. Vite empties the directory on every build and takes the file with it,
# so every target that runs after a build puts it back. Restoring it is
# harmless when it is already there.
dist-keep:
	@mkdir -p internal/front/dist && touch internal/front/dist/.gitkeep

test: test-go

test-go: dist-keep
	go test ./... -count=1

# No frontend yet. Fail loudly rather than succeed silently: a declared but
# empty test target reports success in CI while running nothing.
test-web:
	@echo "test-web: no frontend yet (see Phase D); wire up web/ tests here" >&2; exit 1
