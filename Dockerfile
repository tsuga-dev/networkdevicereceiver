# The binary is cross-compiled by the release workflow and copied in, rather
# than built here: the collector builder generates the sources, and running it
# once beats running it once per architecture.
#
# Build with buildx so TARGETARCH is set:
#   docker buildx build --platform linux/amd64,linux/arm64 .
FROM gcr.io/distroless/static-debian12:nonroot

ARG TARGETARCH

# --chmod because actions/upload-artifact does not preserve the executable bit,
# and this image has no shell to fix it up in.
COPY --chmod=0755 dist/linux_${TARGETARCH}/networkdevicereceiver /networkdevicereceiver

ENTRYPOINT ["/networkdevicereceiver"]
