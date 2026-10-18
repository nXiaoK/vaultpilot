FROM node:20.20.2-bookworm-slim AS frontend
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
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl bzip2 unzip && rm -rf /var/lib/apt/lists/*
WORKDIR /tmp/engines
RUN curl -fsSLO "https://github.com/restic/restic/releases/download/v${RESTIC_VERSION}/restic_${RESTIC_VERSION}_linux_${TARGETARCH}.bz2" && curl -fsSLO "https://github.com/restic/restic/releases/download/v${RESTIC_VERSION}/SHA256SUMS" && grep "restic_${RESTIC_VERSION}_linux_${TARGETARCH}.bz2$" SHA256SUMS | sha256sum -c - && bunzip2 "restic_${RESTIC_VERSION}_linux_${TARGETARCH}.bz2" && install -m 0755 "restic_${RESTIC_VERSION}_linux_${TARGETARCH}" /usr/local/bin/restic
RUN curl -fsSLO "https://downloads.rclone.org/v${RCLONE_VERSION}/rclone-v${RCLONE_VERSION}-linux-${TARGETARCH}.zip" && curl -fsSL "https://downloads.rclone.org/v${RCLONE_VERSION}/SHA256SUMS" -o RCLONE_SHA256SUMS && grep "rclone-v${RCLONE_VERSION}-linux-${TARGETARCH}.zip$" RCLONE_SHA256SUMS | sha256sum -c - && unzip -q "rclone-v${RCLONE_VERSION}-linux-${TARGETARCH}.zip" && install -m 0755 "rclone-v${RCLONE_VERSION}-linux-${TARGETARCH}/rclone" /usr/local/bin/rclone

FROM debian:12.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata && rm -rf /var/lib/apt/lists/* && groupadd -g 1000 vaultpilot && useradd -u 1000 -g vaultpilot -m -s /usr/sbin/nologin vaultpilot && mkdir -p /app/state /app/cache /sources /restore && chown -R vaultpilot:vaultpilot /app /restore
COPY --from=engines /usr/local/bin/restic /usr/local/bin/rclone /usr/local/bin/
COPY --from=backend /out/ /app/
COPY --from=frontend /src/web/dist/ /app/web/dist/
WORKDIR /app
USER 1000:1000
EXPOSE 8080
ENTRYPOINT ["/app/vaultpilot"]
