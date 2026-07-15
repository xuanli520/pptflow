# Repository snapshot preparation

Use only the locked Git executable and the AuthoringSession source identity.
Create a read-only, content-addressed snapshot of the approved
`tower-http` repository commit. Record the exact source URL, full commit,
snapshot digest, and Git evidence. Do not use PATH lookup, mutable branches,
credentials, submodule defaults, or a caller-provided working directory.
