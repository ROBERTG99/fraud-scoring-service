# Vendored protobuf dependencies

`google/api/{annotations,http,field_behavior}.proto` are copied unmodified from
<https://github.com/googleapis/googleapis> (Apache License 2.0; the license header is kept in each file).
They are vendored because the Buf Schema Registry is unreachable from the build environment
(see `docs/DEVELOPMENT.md`). Code is not generated for them: Go uses `google.golang.org/genproto`.
