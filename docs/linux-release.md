# Linux Release Install

This archive contains a statically linked `p2r` binary for Linux.

## Install

```sh
tar -xzf p2r_<version>_linux_amd64.tar.gz
cd p2r_<version>_linux_amd64
./p2r --help
```

To make the binary available system-wide:

```sh
sudo install -m 0755 p2r /usr/local/bin/p2r
p2r --help
```

## Configuration

Start from the included example config when you need a local config file:

```sh
cp p2r.example.yaml .p2r.yaml
p2r scan --path /path/to/projects-qa
```

For browser E2E stages, ensure Docker, Node.js, Playwright browsers, and the
configured Codex CLI are available on the target host.

## Verify

The release script writes a SHA256 file next to the archive. Verify it with:

```sh
sha256sum -c p2r_<version>_linux_amd64.tar.gz.sha256
```
