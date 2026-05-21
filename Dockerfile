# syntax=docker/dockerfile:1.6

FROM golang:1.23-alpine AS builder
WORKDIR /src

# Cache deps
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/openai-exporter .

FROM alpine:3.23
LABEL maintainer="Prezi <opensource@prezi.com>"
LABEL org.opencontainers.image.source="https://github.com/prezi/openai-exporter"
LABEL org.opencontainers.image.description="Prometheus exporter for OpenAI usage and cost metrics"
LABEL org.opencontainers.image.licenses="MIT"

RUN apk add --no-cache ca-certificates && \
    addgroup -S app && adduser -S -G app app

COPY --from=builder /out/openai-exporter /bin/openai-exporter

USER app
EXPOSE 9185
ENTRYPOINT ["/bin/openai-exporter"]
