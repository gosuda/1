# Build context: the root of refs/heads/lander (published by the lander from .github/scripts).
#   docker build -t lander:$(git rev-parse --short HEAD) .
ARG GO_VERSION=1.27.3
FROM golang:${GO_VERSION} AS build
WORKDIR /src
COPY capy ./capy
COPY lander ./lander
RUN cd lander && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/lander ./cmd/lander

FROM docker:cli AS dockercli

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends git openssh-client ca-certificates \
 && rm -rf /var/lib/apt/lists/*
# Go is needed to warm module caches for presubmit (`go mod download`); no repository code runs here.
COPY --from=build /usr/local/go /usr/local/go
COPY --from=dockercli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=build /out/lander /usr/local/bin/lander
ENV PATH=/usr/local/go/bin:$PATH \
    LANDER_STORE=/var/lib/lander/data/store.git
RUN useradd --system --uid 10001 --home-dir /var/lib/lander --create-home lander
USER 10001
VOLUME /var/lib/lander
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/lander"]
CMD ["run"]
