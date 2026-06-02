# syntax=docker/dockerfile:1

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/stargazer-server ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 stargazer \
    && mkdir -p /data \
    && chown stargazer:stargazer /data
COPY --from=build /out/stargazer-server /usr/local/bin/stargazer-server
USER stargazer
ENV DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/stargazer-server"]
