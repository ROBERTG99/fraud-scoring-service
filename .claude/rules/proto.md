---
paths:
  - "proto/**"
  - "buf*.yaml"
---
# Protobuf rules

- Never renumber, reuse or change the type of a field. Remove fields with `reserved`.
- `buf lint` (STANDARD) and `buf breaking --against '.git#branch=main'` must pass.
- Run `make proto` after any change; commit `gen/` and `web/src/api/schema.d.ts` together with the proto.
