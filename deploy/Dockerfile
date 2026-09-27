# syntax=docker/dockerfile:1

FROM node:24.21.0-alpine AS web
WORKDIR /src/web
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

FROM golang:1.27.1-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -tags webui \
    -ldflags "-s -w -X github.com/jlbyh2o/ezdr/internal/version.Version=${VERSION}" \
    -o /out/ezdr-portal ./cmd/ezdr-portal \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/ezdr-portal /usr/local/bin/ezdr-portal
# The data directory must be writable by the nonroot user; named volumes
# inherit this ownership when first created.
COPY --from=build --chown=65532:65532 /out/data /data
ENV EZDR_DATA_DIR=/data \
    EZDR_LISTEN=:8080
EXPOSE 8080/tcp 51820/udp
ENTRYPOINT ["/usr/local/bin/ezdr-portal"]
