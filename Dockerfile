FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine@sha256:f2c872bcee949b7be9c490efd02b48a8a2fbf947cdf533f1c58525ec9d8170fd AS builder

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

FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99

COPY --from=builder /server /server
COPY --from=builder /app/LICENSE /LICENSE
COPY --from=builder /app/NOTICE /NOTICE
COPY --from=builder --chown=65532:65532 /out/data /data

EXPOSE 8080

VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/server", "--healthcheck"]

ENTRYPOINT ["/server"]
