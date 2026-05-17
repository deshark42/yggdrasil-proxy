FROM golang:1.21-alpine AS builder
WORKDIR /build
COPY . .
RUN go build -o yggdrasil-proxy .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=builder /build/yggdrasil-proxy /app/yggdrasil-proxy
COPY config.example.json /app/config.json
WORKDIR /app
EXPOSE 32217
CMD ["./yggdrasil-proxy"]
