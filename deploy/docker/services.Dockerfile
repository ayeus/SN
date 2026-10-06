# One image for every Go service, plus the migration tooling. Each container
# picks its binary with `command:` (see deploy/compose/docker-compose.prod.yml).
#
#   docker build -f deploy/docker/services.Dockerfile -t ayeusann/services .

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY VERSION ./VERSION
COPY gen ./gen
COPY internal ./internal
COPY services ./services
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    for svc in gateway control-api scheduler coordinator inference-gateway trust-engine; do \
      CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
        -ldflags="-s -w -X github.com/ayeus/ayeusann/internal/platform.Version=$(cat VERSION)" -o /out/$svc ./services/$svc || exit 1; \
    done

FROM alpine:3.20 AS migrate
ARG TARGETARCH
ARG MIGRATE_VERSION=v4.18.1
RUN wget -qO- "https://github.com/golang-migrate/migrate/releases/download/${MIGRATE_VERSION}/migrate.linux-${TARGETARCH}.tar.gz" \
    | tar xz -C /usr/local/bin migrate

FROM alpine:3.20
# psql applies the seed files, pg_dump and pg_restore take and check backups;
# wget (busybox) backs the compose healthchecks.
RUN apk add --no-cache ca-certificates tzdata postgresql16-client \
    && adduser -D -H -u 10001 ayeusann
WORKDIR /app
COPY --from=build /out/ /usr/local/bin/
COPY --from=migrate /usr/local/bin/migrate /usr/local/bin/migrate
COPY schema ./schema
COPY web/install ./web/install
COPY deploy/docker/migrate.sh /usr/local/bin/migrate-and-seed
COPY deploy/docker/backup.sh /usr/local/bin/backup
RUN chmod +x /usr/local/bin/migrate-and-seed /usr/local/bin/backup && mkdir -p /app/downloads
ENV INSTALL_DIR=/app/web/install DOWNLOADS_DIR=/app/downloads
USER ayeusann
