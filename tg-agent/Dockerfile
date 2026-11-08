# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /tg-agent .
FROM alpine:3.23
RUN --mount=type=secret,id=proxy_ca \
    if [ -s /run/secrets/proxy_ca ]; then export SSL_CERT_FILE=/run/secrets/proxy_ca; fi; \
    apk add --no-cache bash ca-certificates git openssh-client ripgrep coreutils python3 \
    && addgroup -g 10001 agent && adduser -D -u 10001 -G agent agent \
    && mkdir -p /app/workspace && chown -R agent:agent /app
COPY --from=build /tg-agent /usr/local/bin/tg-agent
COPY --chown=agent:agent skills /app/skills
USER agent
WORKDIR /app
ENTRYPOINT ["tg-agent", "--config", "/app/config.json"]
