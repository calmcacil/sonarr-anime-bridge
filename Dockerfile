FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY LICENSE NOTICE ./

ARG TARGETOS TARGETARCH
ARG VERSION=dev

RUN --mount=type=cache,id=go-build-${TARGETOS}-${TARGETARCH},target=/root/.cache/go-build,sharing=locked \
  CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
  go build -trimpath -buildvcs=false -ldflags="-s -w -X main.version=${VERSION}" -o /server ./cmd/server

RUN mkdir -p /out/data && chmod 0775 /out/data

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329

COPY --from=builder /server /server
COPY --from=builder /app/LICENSE /LICENSE
COPY --from=builder /app/NOTICE /NOTICE
COPY --from=builder --chown=65532:65532 /out/data /data

EXPOSE 8080

VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/server", "--healthcheck"]

ENTRYPOINT ["/server"]
