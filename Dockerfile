FROM       alpine:3.21
MAINTAINER Maxim Pogozhiy <foxdalas@gmail.com>

ARG TARGETARCH

RUN apk add --no-cache ca-certificates
COPY openai-exporter /bin/openai_exporter

ENTRYPOINT ["/bin/openai_exporter"]
EXPOSE     9185
