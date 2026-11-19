FROM scratch
ARG TARGETARCH
COPY dist/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY dist/personal-mail-mcp-linux-${TARGETARCH} /personal-mail-mcp
USER 65532:65532
ENTRYPOINT ["/personal-mail-mcp"]
CMD ["-config", "/config/config.json", "serve"]
