FROM golang:1.27.3-alpine3.24 AS builder
WORKDIR /build
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/yggdrasil-proxy .

FROM alpine:3.24
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 app \
    && adduser -S -D -H -u 10001 -G app app
WORKDIR /app
COPY --from=builder /out/yggdrasil-proxy /app/yggdrasil-proxy
COPY config.example.json /app/config.json
USER 10001:10001
EXPOSE 32217
CMD ["./yggdrasil-proxy"]
