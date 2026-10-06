FROM golang:1.26.6-bookworm@sha256:7939e2c75db3d059fc944bb6464a916d0fa64bd5a3bd7b3528f2a1ac7673a0eb AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
ARG SOURCE_REVISION
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.imageRevision=${SOURCE_REVISION}" -o /controller .

FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251 AS controller
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
# Fixed, signed snapshots prevent apt dependencies drifting on a repeated build.
RUN rm -f /etc/apt/sources.list /etc/apt/sources.list.d/*.sources /etc/apt/sources.list.d/*.list \
 && printf '%s\n' \
 'deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/20261004T000000Z/ bookworm main' \
 'deb [check-valid-until=no] https://snapshot.debian.org/archive/debian-security/20261004T000000Z/ bookworm-security main' > /etc/apt/sources.list \
 && apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /controller /controller
ENTRYPOINT ["/controller"]

FROM node:24.14.0-bookworm-slim@sha256:d8e448a56fc63242f70026718378bd4b00f8c82e78d20eefb199224a4d8e33d8 AS node
FROM ubuntu:24.04@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55 AS runner
ENV DEBIAN_FRONTEND=noninteractive
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
RUN rm -f /etc/apt/sources.list /etc/apt/sources.list.d/*.sources /etc/apt/sources.list.d/*.list \
 && printf '%s\n' \
 'deb [check-valid-until=no] https://snapshot.ubuntu.com/ubuntu/20261004T000000Z/ noble main universe' \
 'deb [check-valid-until=no] https://snapshot.ubuntu.com/ubuntu/20261004T000000Z/ noble-updates main universe' \
 'deb [check-valid-until=no] https://snapshot.ubuntu.com/ubuntu/20261004T000000Z/ noble-security main universe' > /etc/apt/sources.list \
 && apt-get update && apt-get install -y --no-install-recommends ca-certificates curl git jq python3 python3-venv python3-pip postgresql-client iptables libicu74 libssl3t64 libkrb5-3 zlib1g build-essential gh rsync && rm -rf /var/lib/apt/lists/* \
 && useradd -m -u 1001 -s /bin/bash runner
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx
WORKDIR /opt/actions-runner
RUN curl -fsSL https://github.com/actions/runner/releases/download/v2.337.0/actions-runner-linux-arm64-2.337.0.tar.gz -o /runner.tar.gz \
 && printf '%s  %s\n' 9b1dc70626422526e3c94767cf024896beb15da5342a3f4819bf2feac13e0393 /runner.tar.gz | sha256sum -c - \
 && tar -xzf /runner.tar.gz && rm /runner.tar.gz && chown -R runner:runner /home/runner /opt/actions-runner
COPY --from=build /usr/local/go /opt/go
ENV PATH="/opt/go/bin:${PATH}"
ENV GOPROXY=https://goproxy.cn GOSUMDB=sum.golang.org
COPY egress.py firewall.sh runner.sh toolcache-init.sh cache-init.py job-disk-init.py /opt/ci/
COPY cache-env.sh cache-toolcache-init.sh /opt/ci/
COPY workspace-init.py tools-init.py cache-compatibility.json /opt/ci/
COPY ci-postgres.sh /usr/local/bin/ci-postgres
RUN chmod 755 /opt/ci/*.sh
RUN chmod 755 /usr/local/bin/ci-postgres
WORKDIR /home/runner
USER runner
CMD ["/opt/ci/runner.sh"]

# Mirror the exact official PostgreSQL dependency so deployment only needs GHCR.
FROM postgres:17-bookworm@sha256:639ab7ceb90e13123085b741fb31ef493fba25463002f6da665352e7b534b652 AS postgres
