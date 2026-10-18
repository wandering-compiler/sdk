# Protos

Every Go package under `go/pb/` is generated from these files with
`make pb` (protoc and its plugins pinned in `tools/protoc`); `make check-pb`
fails when the committed code differs from what `make pb` writes.

| directory | what it is |
|---|---|
| `w17/` | the annotation vocabulary a project's protos import (`import "w17/db.proto"`) — see `w17/README.md` |
| `w17compiler/` | the compiler service the CLI calls |
| `w17apply/` | the deploy plan and fetch services |
| `w17registry/` | the project registry service |
| `common/distx/` | the distributed-transaction envelope |
| `domains/console/` | the console API messages the CLI uses |

`go/pb/consoleapi/rpc` has no source here: it is the gRPC client for the
console's API, generated from the console's own API definition and refreshed
by the console's maintainers when that API changes.
