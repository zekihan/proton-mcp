# Build binaries with GoReleaser first (make build/docker).
FROM --platform=$BUILDPLATFORM busybox:1.38.0@sha256:a46257e06c82ad85242d0d38077d0a52d9896389ca60a1a59d3463db586e5fa4 AS build
ARG TARGETOS
ARG TARGETARCH
COPY dist/proton-mcp_${TARGETOS}_${TARGETARCH}/proton-mcp /proton-mcp

RUN mkdir -p /data /tmp && chmod 755 /data && chmod 1777 /tmp

FROM ghcr.io/zekihan/distroless:1.0.0-r1@sha256:af9aab3cd99d1f40bca0b21ec21fe4da082422a5bc148b152c35a45802c69a2a AS final
COPY --from=build --chown=25120:25120 /data /data
COPY --from=build --chmod=1777 /tmp /tmp
COPY --from=build /proton-mcp /usr/local/bin/proton-mcp
COPY LICENSE /usr/share/licenses/proton-mcp/LICENSE
USER 25120:25120
EXPOSE 8080 9090
VOLUME ["/data"]
ENV DB_PATH=/data/proton-mcp.db LISTEN_ADDR=:8080
ENTRYPOINT ["proton-mcp"]
LABEL org.opencontainers.image.documentation="https://github.com/zekihan/proton-mcp#readme" \
      org.opencontainers.image.source="https://github.com/zekihan/proton-mcp" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.title="proton-mcp" \
      org.opencontainers.image.description="Owner-approved MCP access to Proton Mail"
