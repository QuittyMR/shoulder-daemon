# advisor-echo — reference OpenAI-compatible advisor used to prove the
# relay -> advisor wiring end to end (see docs/ADVISOR.md).

FROM docker.io/library/golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/advisor-echo .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/advisor-echo /advisor-echo
USER nonroot:nonroot
EXPOSE 9090
HEALTHCHECK --interval=10s --timeout=2s --retries=3 --start-period=5s \
  CMD ["/advisor-echo", "-healthcheck"]
ENTRYPOINT ["/advisor-echo"]
