# Ubuntu/Linux Release

This project ships best as a static Go binary archive for Linux cloud machines.
The binary is built with `CGO_ENABLED=0`, so it does not need libc-specific
packaging or system SQLite libraries.

## Target

- OS: Ubuntu 22.04 LTS or 24.04 LTS
- Architecture: `amd64` (`x86_64`)
- Binary: `p2r`
- Archive: `p2r_<version>_linux_amd64.tar.gz`

## Runtime prerequisites

Install the tools that the QA pipeline shells out to:

```sh
sudo apt-get update
sudo apt-get install -y ca-certificates docker.io docker-compose-plugin python3
docker compose version
python3 --version
```

If you want to run Docker without `sudo`, add your user to the Docker group and
start a new login session:

```sh
sudo usermod -aG docker "$USER"
```

## Build the release

On Linux or CI:

```sh
VERSION=v0.1.0 scripts/build-linux-release.sh
```

On Windows PowerShell:

```powershell
.\scripts\build-linux-release.ps1 -Version v0.1.0
```

The build writes:

```text
dist/p2r_v0.1.0_linux_amd64.tar.gz
dist/p2r_v0.1.0_linux_amd64.tar.gz.sha256
```

## Install on Ubuntu

Copy the archive and checksum to the cloud VM, then install:

```sh
sha256sum -c p2r_v0.1.0_linux_amd64.tar.gz.sha256
tar -xzf p2r_v0.1.0_linux_amd64.tar.gz
sudo install -m 0755 p2r_v0.1.0_linux_amd64/p2r /usr/local/bin/p2r
p2r version
```

## First run

Create a working directory with a `.p2r.yaml` config, then scan and open the
TUI from that directory:

```sh
mkdir -p "$HOME/p2r-work/projects-qa"
cd "$HOME/p2r-work"
cp /path/to/p2r_v0.1.0_linux_amd64/p2r.example.yaml .p2r.yaml
p2r scan
p2r tui
```

For runtime evidence stages, make sure Docker is reachable from the same user:

```sh
docker ps
docker compose version
```

If Docker is unavailable on the VM, run static-only evidence:

```sh
p2r run TASK-20260408-A1B2C3 --static-only
```
