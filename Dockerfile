FROM golang:1.25.0-alpine as builder
WORKDIR /app
COPY . /app
RUN go mod tidy
RUN go build .

FROM ubuntu:latest
WORKDIR /app
RUN apt update && apt upgrade -y
RUN apt install -y clang
COPY --from=builder /app/runtime /app/runtime
COPY --from=builder /app/tin /app
COPY --from=builder /app/examples /app/examples
COPY --from=builder /app/stdlib /app/stdlib
