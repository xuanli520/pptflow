# Generated content review

Review the frozen instruction and task metadata together with the exact
`validated_dockerfile` and host-owned `dockerfile_build_report`. Approve only
when the report passed for those Dockerfile bytes, the content agrees with the
approved proposal, and later verification can run locally without credentials
or runtime network access. Record one durable decision using the locked review
schema; do not alter task files or reinterpret the build report in this gate.
