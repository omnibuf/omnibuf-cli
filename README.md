# omnibuf-cli

[![OmniBuf Spec: v1 DRAFT-1](https://img.shields.io/badge/OmniBuf%20Spec-v1%20DRAFT--1-FF4F18)](https://omnibuf.com/spec)

Command-line tool (`omnibuf`) for inspecting, querying, encoding, and decoding [OmniBuf Wire Specification v1 DRAFT-1](https://omnibuf.com/spec) packets. Full docs live at [omnibuf.com/tooling](https://omnibuf.com/tooling).

## Install

```bash
# Homebrew (macOS & Linux)
brew install omnibuf/tap/omnibuf

# Shell script (macOS & Linux)
curl -fsSL https://omnibuf.com/install.sh | sh

# mise (macOS, Linux & Windows)
mise use -g github:omnibuf/omnibuf-cli

# Go toolchain
go install github.com/omnibuf/omnibuf-cli/cmd/omnibuf@latest
```

## Quick usage

```bash
# Encode JSON to a self-describing OmniBuf packet
echo '{"asn": 64512, "router_id": "edge-fra-01", "location": {"city": "Frankfurt", "rack": 42}}' \
  | omnibuf > peer.obuf

# Decode packet to pretty JSON
omnibuf peer.obuf

# Query with a jq filter (-r for raw string, -c for compact JSON)
omnibuf -r '.location.city' peer.obuf

# Mutate a packet in-flight and re-encode (-e)
omnibuf -e '.location.rack += 1' peer.obuf > peer_updated.obuf
```

## Implementation

- **Auto-detect direction:** Binary OmniBuf input decodes to JSON; JSON input encodes to a self-describing OmniBuf packet with `Field #0` metadata.
- **Built-in `jq` engine:** Filter nested fields, slice arrays with `select(...)`, and transform payloads in place (`-r`, `-c`, `-e`).
- **Hex and base64 I/O:** Inspect or emit hex (`-x` / `--hex`) and base64 (`-b` / `--base64`) directly from stdin or files.
- **Schema-aware mode (`-s` / `--schema`):** Accepts `FileDescriptorSet` files (`.fds.json`, `.fds.pb`, `.fds.pb.gz`) with `-t <TypeName>` to encode, decode, and query schema-typed packets (`omnibuf -s schema.pb.gz -t PeerUpdate '.neighbors[]' peer.obuf`).
- **Prebuilt binaries:** Release archives and SHA-256 checksums are published on [GitHub Releases](https://github.com/omnibuf/omnibuf-cli/releases).

## License

Apache-2.0
