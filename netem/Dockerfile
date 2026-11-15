# Bad-network test image: quicvideod + bots + tc (iproute2).
FROM golang:1.27 AS builder
WORKDIR /app
COPY go.mod go.sum ./
# Vendored module (go.mod replace) must be there for go mod download.
COPY third_party/ third_party/
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/ ./server/cmd ./examples/pub ./examples/netbench ./examples/loadtest \
 && mv /out/cmd /out/quicvideod

FROM debian:bookworm-slim
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends iproute2 procps && rm -rf /var/lib/apt/lists/*
COPY --from=builder /out/ /usr/local/bin/
