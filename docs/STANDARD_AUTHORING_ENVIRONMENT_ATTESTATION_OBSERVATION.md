# Standard Authoring Environment Attestation Observation

Observed on 2026-07-15 for the pending Standard-authoring deployment.  This
is a non-secret inventory only: it is not an operation catalog, lock, or
authorization to run a provider.  Values were obtained from explicit absolute
paths and version commands; no credential or endpoint value was read or
printed.

| Component | Absolute path | Observed version/output | SHA-256 |
| --- | --- | --- | --- |
| Git | `/usr/bin/git` | `git version 2.47.3` | `356db14e102d68a1a37d8a1ac577dfd678d45d46e92f468bef8b7154e7bfdc60` |
| Docker client | `/usr/bin/docker` | `Docker version 29.5.2, build 79eb04c` | `abb24795f58721581130a7d4cca53e80a64099ae40a11bebd02cc2f45b9136b8` |
| Docker server | daemon queried by `docker version` | `29.5.2` | not a regular executable; must be dynamically checked at execution time |
| Node | `/root/.nvm/versions/node/v26.2.0/bin/node` | `v26.2.0` | `030a5e93e4f7a022b12a3ec80fecd22af9614356904a05ece6b1b2dbf4c1f588` |
| Codex JavaScript launcher | `/root/.nvm/versions/node/v26.2.0/lib/node_modules/@openai/codex/bin/codex.js` | `codex-cli 0.133.0` | `aa3c64b122c9d06bf48eaf988f5970aa69556d69506c3118cf07d10b2401b48a` |
| Harbor launcher | `/root/.local/share/uv/tools/harbor/bin/harbor` | `0.18.0` | `9b0852df4c749ab9431b7aff6b2f1b1de8b7365ee6a513cdbd7573a1678d4f97` |

The approved source identity for the first authoring session is
`https://github.com/tower-rs/tower-http.git` at
`f066e10ebc07ea9050a2ce4576315abfa568edf4`.  That source fact is distinct
from host executable attestation: the final Source record must still persist a
read-only snapshot artifact and its content fingerprint.

## Docker image observation

The local Docker image list currently includes
`rust:1.96.0-bookworm@sha256:5e2214abe154fe26e39f64488952e5c991eeed1d6d6da7cc8381ae83927f0cfc`.
This is merely an observed local image.  It is **not** approved for a generated
task, and it must not be silently selected by a Standard handler.  A final
task-specific Docker policy must pin the exact image(s) it permits and verify
that a generated Dockerfile uses those digest references.  Unknown tags,
unlocked pulls, and mutable `latest` references must fail closed.

## Required dynamic checks

A future lock-backed attestor must, immediately before each operation:

1. verify each locked file is a regular non-symlink file and recompute its
   SHA-256;
2. execute only the locked binary's bounded version probe under a controlled
   environment, without inheriting arbitrary model/provider configuration;
3. verify the Docker daemon is reachable and reports the locked protocol
   version before Docker-dependent stages; and
4. verify the Codex App Server launcher, Node, `CODEX_HOME`, model
   `gpt-5.5`, workspace-write sandbox, and disabled network policy through
   the existing typed Codex attestor before an `agent.turn`.

Neither this inventory nor its paths may be used as a PATH fallback.  A change
to any observed fact requires a reviewed lock revision and a newly built local
package.
