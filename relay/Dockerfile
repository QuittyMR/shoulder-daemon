# shoulderd — shoulder-daemon relay. Multi-stage build; the Go source for
# relay/cmd/shoulderd does not exist yet (owned by a separate workstream),
# so this Dockerfile is written to build once it lands. Syntax-checked with
# `docker build --check` / `hadolint`; not built here.

FROM docker.io/library/golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/shoulderd ./cmd/shoulderd

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/shoulderd /shoulderd
USER nonroot:nonroot
EXPOSE 8787
ENTRYPOINT ["/shoulderd"]
