# S7 service image: s7-inplace host + built-in v1 module (+ s7-load, s7-patchctl).
#   docker build -f deploy/s7/Dockerfile -t dimp/s7-inplace:dev .
# The image contains ONLY the built-in module v1 (trusted via the image).
# Patches (v2, v3, ...) arrive later as signed bundles; the trusted public
# key is mounted at runtime, never baked in.
FROM golang:1.25 AS build
ENV GOFLAGS=-buildvcs=false CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal/ internal/
COPY cmd/s7-inplace/ cmd/s7-inplace/
COPY cmd/s7-patchctl/ cmd/s7-patchctl/
COPY cmd/s7-load/ cmd/s7-load/
RUN go build -trimpath -o /out/s7-inplace ./cmd/s7-inplace \
 && go build -trimpath -o /out/s7-patchctl ./cmd/s7-patchctl \
 && go build -trimpath -o /out/s7-load ./cmd/s7-load \
 && mkdir -p /out/modules \
 && GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -ldflags="-s -w" -o /out/modules/v1.wasm ./cmd/s7-inplace/guest/v1

FROM alpine:3.20
RUN adduser -D -u 10001 app
COPY --from=build /out/ /app/
USER 10001
EXPOSE 8080 9090
ENTRYPOINT ["/app/s7-inplace"]
CMD ["-addr", ":8080", "-admin-addr", ":9090", "-module-dir", "/app/modules", "-trusted-keys", "/etc/dimp/keys", "-admin-token-file", "/etc/dimp/token/token", "-contract-upgrade=false"]
