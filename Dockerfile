FROM golang:1.26.3-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY controller.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /controller .

FROM debian:bookworm-slim AS controller
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /controller /controller
ENTRYPOINT ["/controller"]

FROM node:24.14.0-bookworm-slim AS node
FROM ubuntu:24.04 AS runner
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl git jq python3 python3-venv python3-pip postgresql-client iptables libicu74 libssl3t64 libkrb5-3 zlib1g build-essential gh && rm -rf /var/lib/apt/lists/* \
 && useradd -m -u 1001 -s /bin/bash runner
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx
WORKDIR /home/runner
RUN curl -fsSL https://github.com/actions/runner/releases/download/v2.337.0/actions-runner-linux-arm64-2.337.0.tar.gz -o /runner.tar.gz \
 && printf '%s  %s\n' 9b1dc70626422526e3c94767cf024896beb15da5342a3f4819bf2feac13e0393 /runner.tar.gz | sha256sum -c - \
 && tar -xzf /runner.tar.gz && rm /runner.tar.gz && chown -R runner:runner /home/runner
COPY egress.py firewall.sh runner.sh /opt/ci/
RUN chmod 755 /opt/ci/*.sh
USER runner
CMD ["/opt/ci/runner.sh"]
