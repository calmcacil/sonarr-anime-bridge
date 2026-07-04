FROM --platform=$BUILDPLATFORM golang:1.26.4-alpine AS builder

ARG TARGETOS TARGETARCH
ARG VERSION=dev

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
  go build -ldflags="-s -w -X main.version=${VERSION}" -o /server ./cmd/server

FROM alpine:3.24

RUN apk add --no-cache ca-certificates su-exec

COPY --from=builder /server /server
COPY entrypoint.sh /entrypoint.sh

EXPOSE 8080

VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD /server --healthcheck || exit 1

ENTRYPOINT ["/entrypoint.sh"]
