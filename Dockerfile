FROM golang:1.25.0-bookworm AS builder
WORKDIR /app
COPY . /app
RUN go mod tidy
ENV CGO_ENABLED=1
RUN go build .

FROM ubuntu:latest
WORKDIR /app
RUN apt update && apt upgrade -y
RUN apt install -y clang libssl-dev libpcre2-dev
COPY --from=builder /app/runtime /app/runtime
COPY --from=builder /app/tin /app
COPY --from=builder /app/examples /app/examples
COPY --from=builder /app/stdlib /app/stdlib
