FROM node:22.23.3-bookworm-slim AS frontend
WORKDIR /src/web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.26.9-bookworm AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/vaultpilot ./cmd/vaultpilot && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/healthcheck ./cmd/healthcheck && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/vprescue ./cmd/vprescue

FROM debian:12.12-slim AS engines
ARG TARGETARCH
ARG RESTIC_VERSION=0.18.1
ARG RCLONE_VERSION=1.70.3
ARG RESTIC_SHA256_amd64=680838f19d67151adba227e1570cdd8af12c19cf1735783ed1ba928bc41f363d
ARG RESTIC_SHA256_arm64=87f53fddde38764095e9c058a3b31834052c37e5826d2acf34e18923c006bd45
ARG RCLONE_SHA256_amd64=7d69057e69385f6514a9684c7eaa424d972096b130284bb34dd967c4ed4f9dad
ARG RCLONE_SHA256_arm64=1b08be34622f1f9bb9b069a85d036bba822b690790215c18a9418dbaf19505fe
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl bzip2 unzip && rm -rf /var/lib/apt/lists/*
WORKDIR /tmp/engines
RUN case "$TARGETARCH" in amd64) checksum="$RESTIC_SHA256_amd64" ;; arm64) checksum="$RESTIC_SHA256_arm64" ;; *) exit 1 ;; esac && curl -fsSLO "https://github.com/restic/restic/releases/download/v${RESTIC_VERSION}/restic_${RESTIC_VERSION}_linux_${TARGETARCH}.bz2" && printf '%s  %s\n' "$checksum" "restic_${RESTIC_VERSION}_linux_${TARGETARCH}.bz2" | sha256sum -c - && bunzip2 "restic_${RESTIC_VERSION}_linux_${TARGETARCH}.bz2" && install -m 0755 "restic_${RESTIC_VERSION}_linux_${TARGETARCH}" /usr/local/bin/restic
RUN case "$TARGETARCH" in amd64) checksum="$RCLONE_SHA256_amd64" ;; arm64) checksum="$RCLONE_SHA256_arm64" ;; *) exit 1 ;; esac && curl -fsSLO "https://downloads.rclone.org/v${RCLONE_VERSION}/rclone-v${RCLONE_VERSION}-linux-${TARGETARCH}.zip" && printf '%s  %s\n' "$checksum" "rclone-v${RCLONE_VERSION}-linux-${TARGETARCH}.zip" | sha256sum -c - && unzip -q "rclone-v${RCLONE_VERSION}-linux-${TARGETARCH}.zip" && install -m 0755 "rclone-v${RCLONE_VERSION}-linux-${TARGETARCH}/rclone" /usr/local/bin/rclone

FROM debian:12.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata tini && rm -rf /var/lib/apt/lists/* && groupadd -g 1000 vaultpilot && useradd -u 1000 -g vaultpilot -m -s /usr/sbin/nologin vaultpilot && mkdir -p /app/state /app/cache /sources /restore && chown -R vaultpilot:vaultpilot /app /restore
COPY --from=engines /usr/local/bin/restic /usr/local/bin/rclone /usr/local/bin/
COPY --from=backend /out/ /app/
COPY --from=frontend /src/web/dist/ /app/web/dist/
WORKDIR /app
USER 1000:1000
EXPOSE 8080
ENTRYPOINT ["/usr/bin/tini", "--", "/app/vaultpilot"]
