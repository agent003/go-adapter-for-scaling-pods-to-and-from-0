# syntax=docker/dockerfile:1.7

# ---- build stage ----
FROM --platform=$BUILDPLATFORM golang:1.21-alpine AS builder

WORKDIR /src

# Cache module downloads in their own layer. go.sum is optional on a fresh
# checkout; the glob keeps the COPY working in either case.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown

RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
        -trimpath \
        -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
        -o /out/proxy .

# ---- runtime stage ----
# distroless static + nonroot is smaller than alpine and ships no shell, so
# the attack surface is minimal. The binary is statically linked (CGO=0).
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/proxy /proxy

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/proxy"]
