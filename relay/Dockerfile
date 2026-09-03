# shoulderd, the shoulder-daemon relay. Cross-compiled on the build host rather
# than emulated: a stdlib-only Go binary needs nothing from the target
# platform, and QEMU makes the compile ten times slower for no gain.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27-alpine AS build
ARG TARGETOS
ARG TARGETARCH
# The tag, passed in by the release pipeline. A COPY has no VCS or module
# metadata, so without this the binary reports itself as devel.
ARG VERSION=devel
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags="-s -w -X main.buildVersion=${VERSION} -X main.buildOrigin=container" \
      -o /out/shoulderd ./cmd/shoulderd

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="shoulder-daemon" \
      org.opencontainers.image.description="Context advisor for coding agents" \
      org.opencontainers.image.source="https://github.com/QuittyMR/shoulder-daemon" \
      org.opencontainers.image.url="https://gitlab.com/quittymr/shoulder-daemon" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/shoulderd /shoulderd
USER nonroot:nonroot
EXPOSE 8787
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/shoulderd", "doctor", "--liveness", "--addr", "http://127.0.0.1:8787"]
ENTRYPOINT ["/shoulderd"]
