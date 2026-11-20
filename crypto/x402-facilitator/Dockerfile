FROM golang:1.26.4 AS builder

WORKDIR /app

COPY . .
RUN cd cmd && CGO_ENABLED=0 go build -o /app/facilitator ./facilitator

FROM debian:stable-slim

RUN apt update -y && apt install -y ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /app/facilitator /app