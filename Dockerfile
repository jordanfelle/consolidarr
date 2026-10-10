# syntax=docker/dockerfile:1
FROM node:22-alpine AS web
WORKDIR /w
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /w/dist ./web/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/consolidarr ./cmd/consolidarr

FROM alpine:3.21
# ffprobe is used to verify downloaded packs (DRM / undecodable video) before old files are deleted.
RUN apk add --no-cache ca-certificates ffmpeg tzdata && adduser -D -u 1000 app && mkdir /data && chown app /data
COPY --from=build /out/consolidarr /usr/local/bin/consolidarr
USER app
ENV CONSOLIDARR_DATA=/data CONSOLIDARR_LISTEN=:7480
VOLUME /data
EXPOSE 7480
HEALTHCHECK --interval=30s --timeout=5s CMD wget -qO- http://127.0.0.1:7480/healthz || exit 1
ENTRYPOINT ["consolidarr"]
