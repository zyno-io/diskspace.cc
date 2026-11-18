# SPDX-License-Identifier: MIT
FROM golang:1.27-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS limitproxy
WORKDIR /src
COPY limitproxy/main.go .
RUN go mod init limitproxy && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /limitproxy .

FROM docker:29-cli@sha256:34507eb5069d8be82c95dd3b6987d4250fd5fee49b82e2509b7d7a7030740d6d
RUN apk add --no-cache webhook util-linux-misc
COPY --from=limitproxy --chmod=755 /limitproxy /usr/local/bin/limitproxy
COPY hooks.json.tmpl /etc/diskspace-deploy/hooks.json.tmpl
COPY --chmod=755 entrypoint.sh /usr/local/bin/diskspace-deploy-entrypoint
COPY --chmod=755 update.sh /usr/local/bin/diskspace-deploy
EXPOSE 9000
ENTRYPOINT ["/usr/local/bin/diskspace-deploy-entrypoint"]
