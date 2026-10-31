# Build from this directory:  docker build -t ghcr.io/<org>/yourwhatsapp-relay:<version> .
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/relay ./cmd/relay
RUN mkdir -p /data

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/relay /relay
# Pre-create /data owned by the nonroot user so a fresh named volume inherits it.
COPY --from=build --chown=65532:65532 /data /data
USER 65532:65532
ENV RELAY_HOST=0.0.0.0 RELAY_DATA_DIR=/data
# The data directory holds relay.db (device keys). It must be a persistent volume.
VOLUME /data
EXPOSE 8800
# Distroless has no shell or curl, so the binary probes itself.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/relay", "healthcheck"]
ENTRYPOINT ["/relay"]
