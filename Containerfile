# Builds the same static binary as `make build` with nothing but podman on the
# host: `make container-build` runs this and exports only the final stage, so
# ./tmux-web lands in the working tree and no image or container is left to
# look after.

FROM docker.io/library/node:24-slim AS front
RUN corepack enable
WORKDIR /src/web
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
# `tsc -b` also checks tsconfig.integrations.json, which reaches out to these.
COPY internal/integrations/ /src/internal/integrations/
# vite.config.ts writes to ../internal/front/dist.
RUN pnpm build

FROM docker.io/library/golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY --from=front /src/internal/front/dist/ internal/front/dist/
# CGO_ENABLED=0 for the reason the Makefile gives. -buildvcs=false because
# .git is not in the build context.
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -o /out/tmux-web ./cmd/tmux-web

FROM scratch AS export
COPY --from=build /out/tmux-web /tmux-web
