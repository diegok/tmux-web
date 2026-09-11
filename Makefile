.PHONY: build test test-go test-web test-e2e front dist-keep

# CGO_ENABLED=0 makes this a genuinely static binary. Without it Go links
# against the build machine's libc for DNS and user lookups, so a binary built
# here fails on a box with an older glibc -- which defeats the reason this is
# written in Go at all. The cost is that os/user reads /etc/passwd directly
# instead of going through NSS, so a host that resolves its users over LDAP
# reports "unknown" in the sidebar footer; the footer already degrades to that
# and nothing else depends on the name.
build: front
	CGO_ENABLED=0 go build -o tmux-web ./cmd/tmux-web

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

test: test-go test-web

test-go: dist-keep
	go test ./... -count=1 -race

test-web:
	cd web && pnpm install && pnpm test

# The end-to-end suite: a real browser against a real daemon and a real tmux
# server on a socket of its own. Deliberately not part of `test`: it builds the
# binary (see e2e/global-setup.ts), starts processes and needs a browser
# downloaded, none of which belong in the loop a person runs on every save.
test-e2e:
	pnpm install
	pnpm exec playwright install chromium
	pnpm exec playwright test
