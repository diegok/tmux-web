.PHONY: help build container-build test test-go test-web test-e2e front dist-keep

# A bare `make` lists the targets instead of building anything.
.DEFAULT_GOAL := help

# CGO_ENABLED=0 makes this a genuinely static binary. Without it Go links
# against the build machine's libc for DNS and user lookups, so a binary built
# here fails on a box with an older glibc -- which defeats the reason this is
# written in Go at all. The cost is that os/user reads /etc/passwd directly
# instead of going through NSS, so a host that resolves its users over LDAP
# reports "unknown" in the sidebar footer; the footer already degrades to that
# and nothing else depends on the name.
build: front ## Build the static ./tmux-web binary (needs Go and pnpm)
	CGO_ENABLED=0 go build -o tmux-web ./cmd/tmux-web

# Same binary as `build`, but Go and pnpm live in a container instead of on
# this machine. Only the Containerfile's last stage is exported: ./tmux-web.
container-build: ## Build ./tmux-web inside podman (needs only podman)
	podman build --target export --output type=local,dest=. -f Containerfile .

front: ## Build the frontend into internal/front/dist
	cd web && pnpm install && pnpm build
	$(MAKE) dist-keep

# internal/front/dist/.gitkeep is what lets `//go:embed all:dist` compile on a
# clone where the frontend has never been built -- an embed pattern that
# matches nothing is a compile error, and dist is build output that stays out
# of git. Vite empties the directory on every build and takes the file with it,
# so every target that runs after a build puts it back. Restoring it is
# harmless when it is already there.
dist-keep: ## Restore internal/front/dist/.gitkeep
	@mkdir -p internal/front/dist && touch internal/front/dist/.gitkeep

test: test-go test-web ## Run the Go and web unit tests

test-go: dist-keep ## Run the Go tests with -race
	go test ./... -count=1 -race

test-web: ## Run the web (vitest) tests
	cd web && pnpm install && pnpm test

# The end-to-end suite: a real browser against a real daemon and a real tmux
# server on a socket of its own. Deliberately not part of `test`: it builds the
# binary (see e2e/global-setup.ts), starts processes and needs a browser
# downloaded, none of which belong in the loop a person runs on every save.
test-e2e: ## Run the Playwright end-to-end suite
	pnpm install
	pnpm exec playwright install chromium
	pnpm exec playwright test

# Lists every target that carries a `## description` on its own line.
help: ## Show this list of targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_-]+:.*## / {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
