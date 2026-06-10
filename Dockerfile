FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=$VERSION" \
    -o /out/sync2connect ./cmd/sync2connect

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/sync2connect /usr/local/bin/sync2connect

# Mount a volume at /data containing config.yaml; tokens and sync state are
# stored alongside it.
ENV SYNC2CONNECT_CONFIG=/data/config.yaml \
    SYNC2CONNECT_STATE_DIR=/data
VOLUME /data

ENTRYPOINT ["/usr/local/bin/sync2connect"]
CMD ["sync"]
