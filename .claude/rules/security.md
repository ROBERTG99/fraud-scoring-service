---
paths:
  - "internal/auth/**"
  - "internal/api/**"
  - "src/fraud_summarizer/redact.py"
  - "docs/security.md"
---
# Security rules

- `Score` requires the service bearer token; CaseService requires a dev-issuer JWT; RBAC in `internal/auth/rbac.go`.
- Body ≤ 64 KB, 20 req/s per user, currency and MCC allowlists.
- No PII at INFO logs; device IDs salted-hashed; IP truncated to /24; redact before any provider call.
- Secrets only from the environment; never commit `.env`.
