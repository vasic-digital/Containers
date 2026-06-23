# syntax=docker/dockerfile:1
FROM gcr.io/distroless/static:nonroot

COPY bin/discovery /discovery

EXPOSE 50052

# DS-0002: make the non-root user explicit. The distroless:nonroot base already
# runs as uid 65532; this USER line states it for the scanner + future readers.
USER nonroot:nonroot

ENTRYPOINT ["/discovery"]
