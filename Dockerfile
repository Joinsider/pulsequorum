# syntax=docker/dockerfile:1

# --- Frontend ---------------------------------------------------------------
FROM --platform=$BUILDPLATFORM node:24-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npx ng build

# --- Backend ----------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.* ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY --from=web /src/web/dist/pulsequorum-web/browser/ internal/webui/dist/

ARG TARGETOS TARGETARCH
ARG VERSION=dev COMMIT=unknown DATE=unknown
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w \
      -X github.com/joinsider/pulsequorum/internal/version.Version=${VERSION} \
      -X github.com/joinsider/pulsequorum/internal/version.Commit=${COMMIT} \
      -X github.com/joinsider/pulsequorum/internal/version.Date=${DATE}" \
    -o /out/pulsequorum ./cmd/pulsequorum && mkdir -p /out/data

# --- Runtime ----------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/pulsequorum /pulsequorum
# Empty data dir owned by nonroot, so a fresh named volume is writable.
COPY --from=build --chown=65532:65532 /out/data /data
ENV DATA_DIR=/data
EXPOSE 8080 9443
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/pulsequorum", "healthcheck"]
ENTRYPOINT ["/pulsequorum"]
