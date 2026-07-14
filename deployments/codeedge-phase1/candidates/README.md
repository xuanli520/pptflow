# CodeEdge Phase-1 Discovery Candidates

Files in this directory are observations collected from a specific local
installation. They are deliberately not deployment catalogs, locks, or
execution authority.

An operator must review the observations, supply every unresolved contract,
and generate a separate canonical `operation-catalog.v1.json` plus
`operation-catalog.lock.json` before any external provider operation can be
enabled. The command composition rejects missing or unapproved production
inputs rather than using this directory as a fallback.
