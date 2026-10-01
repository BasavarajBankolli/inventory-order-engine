# ---------- Stage 1: build ----------
# A full Go toolchain image, used only to compile. It is thrown away later.
FROM golang:1.26-alpine AS build
WORKDIR /src

# Copy go.mod/go.sum first and download modules in their own layer.
# Docker caches this layer, so dependencies are only re-downloaded when
# go.mod/go.sum change, not on every code edit.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 -> a static binary that runs on any Linux without libc.
# -trimpath and -ldflags="-s -w" make the binaries smaller and reproducible.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker

# ---------- Stage 2: run ----------
# A small image containing only Alpine and our three binaries (~85 MB),
# instead of the build image with the whole Go toolchain (~360 MB + sources).
# Alpine keeps a shell and wget, which is handy for learning/debugging
# (`docker compose exec api sh`) and for the healthcheck.
FROM alpine:3.22
WORKDIR /app

# Never run as root inside the container.
RUN adduser -D -u 10001 appuser
USER appuser

COPY --from=build /out/api /out/migrate /out/worker /app/

EXPOSE 8080
CMD ["/app/api"]
