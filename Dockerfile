# Build llm-proxy as a static binary and ship it on a base with nothing else
# on it.
#
# The image carries one executable and a set of root certificates (the
# upstream is HTTPS). It has no shell; `llm-proxy login` is the same binary,
# run with `kubectl exec`.

# The build stage runs on the builder's own platform and cross-compiles, so
# the arm64 image is built without emulation.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src

COPY go.mod go.sum ./
COPY cmd/ cmd/
COPY internal/ internal/

# CGO off makes the result static, which is what lets it run on a base image
# with no libc. The caches are mounts rather than layers so that rebuilding
# after an edit does not start from an empty module cache.
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/llm-proxy ./cmd/llm-proxy

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /out/llm-proxy /llm-proxy
# 65532 is distroless's "nonroot"; the Deployment names the same number so the
# manifest and the image cannot disagree about who this runs as.
USER 65532:65532
ENTRYPOINT ["/llm-proxy"]
CMD ["serve"]
