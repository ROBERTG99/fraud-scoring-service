---
name: add-endpoint
description: Add an RPC with HTTP mapping, RBAC, OpenAPI and TypeScript types. Use for any new API surface.
---
# Add an endpoint

1. Add the RPC and messages to `proto/fraud/v1/*.proto` with a `google.api.http` option. Never renumber.
2. `make proto` (buf lint, breaking, codegen, OpenAPI v3, `web/src/api/schema.d.ts`).
3. Add the method to the RBAC table in `internal/auth/rbac.go` and a test in `rbac_test.go`.
4. Implement the handler in `internal/api/` with deadlines; map errors to gRPC codes.
5. Add a contract test (Schemathesis covers the OpenAPI automatically) and a TanStack Query hook in `web/src/api/`.
