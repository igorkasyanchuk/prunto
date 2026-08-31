# Two stages, and the second one is empty. No libvips, no cgo, nothing to CVE-scan but the
# binary itself - which is the whole point of dropping the decoder.
# Minor-only pin, matching go.mod's declared line: the tag floats across patch releases,
# so a --pull build ships the patched stdlib that CI's govulncheck gate verified. Bump
# together with go.mod - the image sets GOTOOLCHAIN=local, so a go.mod ahead of the image
# fails the build instead of downloading a newer toolchain.
FROM golang:1.25-alpine AS build

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
