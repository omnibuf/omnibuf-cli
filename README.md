# OmniBuf CLI (`omnibuf`)

[![CI](https://github.com/omnibuf/omnibuf-cli/actions/workflows/ci.yml/badge.svg)](https://github.com/omnibuf/omnibuf-cli/actions/workflows/ci.yml)
[![Website](https://img.shields.io/badge/website-omnibuf.com-111111?style=flat&labelColor=111111&color=D4FF33)](https://omnibuf.com)

A `jq`-style command-line tool for inspecting, querying, encoding, decoding, and conformance-testing **OmniBuf** binary messages.

- **Auto-detects direction**: Pipe an OmniBuf packet in to get pretty JSON out; pipe JSON in to get a self-describing OmniBuf binary packet (`Field #0` `SchemaMeta`) out.
- **Built-in `jq` filter engine**: Query nested fields, filter arrays with `select(...)`, and reshape messages on the fly (`omnibuf -r '.location.city' packet.obuf`).
- **Zero `.proto` required (or bring a `FileDescriptorSet`)**: Works out of the box with no schema files using OmniBuf's 16 explicit wire types and `Field #0` metadata, or accepts `.fds.json` / `.fds.pb` / `.fds.pb.gz` schemas via `-s`.
- **Self-contained conformance runner**: Includes the 584-case cross-language conformance suite (`omnibuf conformance <adapter-binary>`).

---

## Install

### Homebrew (macOS & Linux)

```bash
brew install omnibuf/tap/omnibuf
```

### Shell Script (macOS & Linux)

```bash
curl -fsSL https://omnibuf.com/install.sh | sh
```

### `mise` (macOS, Linux & Windows)

```bash
mise use -g github:omnibuf/omnibuf-cli
```

### Go Toolchain

```bash
go install github.com/omnibuf/omnibuf-cli/cmd/omnibuf@latest
```

Prebuilt archives and SHA-256 checksums are also published on [GitHub Releases](https://github.com/omnibuf/omnibuf-cli/releases).

---

## Quick Usage (`jq`-Style)

```bash
# 1. Encode JSON into a self-describing OmniBuf packet (with Field #0 field names)
echo '{"asn": 64512, "router_id": "edge-fra-01", "active": true, "location": {"city": "Frankfurt", "rack": 42}}' \
  | omnibuf > peer.obuf

# 2. Decode any OmniBuf packet to pretty JSON (no schema required)
omnibuf peer.obuf

# 3. Query with a jq filter expression (-r for raw string output, -c for compact JSON)
omnibuf -r '.location.city' peer.obuf
# Frankfurt

# 4. Mutate an OmniBuf packet in-flight with a jq expression and re-encode (-e)
omnibuf -e '.location.rack += 1' peer.obuf > peer_updated.obuf

# 5. Inspect or emit hex (-x / --hex) or base64 (-b / --base64)
echo '{"asn": 64512, "router_id": "edge-fra-01"}' | omnibuf -x
omnibuf -x <<< "19305d2c000a02020661736e0412726f757465725f69642002fc7d002a16656467652d6672612d3031"
```

---

## Schema-Aware Mode (`-s` / `--schema`)

Pass a `FileDescriptorSet` (`.json`, `.pb`, or `.pb.gz` produced by `buf build -o schema.pb.gz` or `protoc -o schema.pb`):

```bash
# Encode JSON using a Protobuf descriptor
omnibuf -s schema.pb.gz -t PeerUpdate input.json > peer.obuf

# Decode and query using a Protobuf descriptor
omnibuf -s schema.pb.gz -t PeerUpdate '.neighbors[] | select(.established) | .router' peer.obuf
```

---

## Cross-Language Conformance Suite

```bash
omnibuf conformance ./bin/conformance-go
omnibuf conformance node ../omnibuf-es/packages/omnibuf-node/dist/bin/conformance-es.js
```

## License

Released under the [MIT License](./LICENSE).
