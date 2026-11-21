# syntax=docker/dockerfile:1.4

FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -mod=readonly -trimpath -ldflags="-w -s" -o /out/mail-router ./cmd/mail-router

# Static, non-root runtime with CA certificates and nothing else.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/mail-router /mail-router
EXPOSE 8080
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 CMD ["/mail-router", "-healthcheck"]
ENTRYPOINT ["/mail-router"]
