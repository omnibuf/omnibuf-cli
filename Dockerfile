# syntax=docker/dockerfile:1
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/omnibuf ./cmd/omnibuf

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/omnibuf /usr/local/bin/omnibuf
ENTRYPOINT ["/usr/local/bin/omnibuf"]
