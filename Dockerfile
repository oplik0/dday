# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build
WORKDIR /src
# BuildKit cache mounts keep the Go build cache and module cache across builds,
# so the huge modernc.org/sqlite package is compiled once (~30s) instead of on
# every image build. `make up` / compose use BuildKit, so this just works.
ENV GOCACHE=/root/.cache/go-build GOMODCACHE=/go/pkg/mod
# Cache modules first.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /dday .
# Also build the Matrix bot — a separate long-lived process. Both binaries ship
# in the final image; compose picks which one runs per service via entrypoint.
# It reuses the same cached build artifacts (modernc already compiled above).
# `-tags goolm` selects pure-Go olm/megolm so the bot can read and answer in
# end-to-end encrypted rooms while still linking statically at CGO_ENABLED=0
# (without the tag mautrix pulls in libolm via cgo and would not link).
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux go build -tags goolm -trimpath -ldflags="-s -w" -o /bot ./cmd/bot
# Stage the DB directory here so it can be copied with nonroot ownership below;
# the distroless image has no shell to mkdir/chown at build time.
RUN mkdir -p /data

FROM gcr.io/distroless/static:nonroot
COPY --from=build /dday /dday
COPY --from=build /bot /bot
# /data holds the SQLite DB. Owning it as nonroot (uid 65532) means a fresh
# named volume mounted here inherits that ownership, so the nonroot process can
# write dday.db (+ WAL/SHM) without any host-side chown.
COPY --from=build --chown=65532:65532 /data /data
EXPOSE 3329
USER nonroot:nonroot
# The distroless image has no shell/wget, so probe via the binary's own
# -healthcheck flag (it GETs /healthz on localhost and exits 0/1).
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 CMD ["/dday", "-healthcheck"]
ENTRYPOINT ["/dday"]
