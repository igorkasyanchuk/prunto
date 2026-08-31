# Two stages, and the second one is empty. No libvips, no cgo, nothing to CVE-scan but the
# binary itself - which is the whole point of dropping the decoder.
# Unpinned on purpose: the binary ships this toolchain's stdlib, and govulncheck in CI
# flags stdlib vulns fixed only in newer Go patches — a pinned stale image would ship them.
FROM golang:alpine AS build

WORKDIR /src
RUN apk add --no-cache ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /prunto .

# Created here because scratch has no shell to mkdir with.
RUN mkdir -p /data && chown 65532:65532 /data

FROM scratch

# Needed to reach the bucket and to fetch the CDN header probe at boot.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=65532:65532 /data /data
COPY --from=build /prunto /prunto

USER 65532:65532
ENV DATA_DIR=/data PORT=3000
EXPOSE 3000
VOLUME ["/data"]

ENTRYPOINT ["/prunto"]
