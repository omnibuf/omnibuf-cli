# List available recipes
default:
    @just --list

# Build the omnibuf CLI binary into ./bin/omnibuf
build:
    @mkdir -p bin
    go build -o bin/omnibuf ./cmd/omnibuf

# Run all unit and golden tests
test:
    go test -race -v ./...

# Lint Go code using golangci-lint
lint:
    golangci-lint run ./...

# Format Go code using golangci-lint fmt
fmt:
    golangci-lint fmt

# Check Go code formatting without modifying files
fmt-check:
    golangci-lint fmt --diff

# Run full CI checks
ci: fmt-check lint test build
