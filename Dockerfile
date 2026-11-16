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

FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931

COPY --from=builder /server /server
COPY --from=builder /app/LICENSE /LICENSE
COPY --from=builder /app/NOTICE /NOTICE
COPY --from=builder --chown=65532:65532 /out/data /data

EXPOSE 8080

VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/server", "--healthcheck"]

ENTRYPOINT ["/server"]
