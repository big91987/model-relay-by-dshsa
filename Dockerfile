# syntax=docker/dockerfile:1

FROM node:22-alpine AS web
WORKDIR /src
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml ./web/
RUN cd web && pnpm install --frozen-lockfile
COPY web ./web
RUN mkdir -p /src/internal/server
RUN cd web && pnpm build

FROM golang:1.25-alpine AS build
ARG VERSION=0.1.0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY --from=web /src/internal/server/dist ./internal/server/dist
RUN mkdir -p /out && CGO_ENABLED=0 go build -tags embed -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/gateway ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates \
    && addgroup -S app \
    && adduser -S -G app app \
    && mkdir -p /data \
    && chown app:app /data
COPY --from=build /out/gateway /usr/local/bin/gateway
USER app
WORKDIR /app
ENV PORT=8080 \
    DATA_DIR=/data
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/gateway"]
