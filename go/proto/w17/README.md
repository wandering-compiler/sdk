# The w17 proto vocabulary

> Published to `go/proto/w17/` of the SDK repository by
> `scripts/publish-sdk.sh`. Its source is `scripts/sdk-proto-README.md` in the
> source repository — beside the publisher that owns it, because neither `sdk/`
> nor `proto/w17/` can hold it without a second copy appearing somewhere.

These are the annotation definitions a w17 project writes against — the same ones
your protos already import:

```proto
import "w17/db.proto";
import "w17/field.proto";
import "w17/contrib.proto";
```

Until now you wrote those imports and never had the files: the compiler handed
them over at codegen time. They ship here so that **anything that compiles proto
outside the compiler can resolve them** — which is what a plugin author needs.

## What is here

| file | what it declares |
|---|---|
| `db.proto` | tables, methods, DQL, indexes, upsert |
| `field.proto` | column types, `unique`, `max_len`, defaults |
| `rest.proto` | the REST / SSE / WS surface |
| `admin.proto` | admin pages, columns, references, widgets |
| `event.proto`, `event_subscribers.proto` | the event surface |
| `module.proto`, `domain.proto` | connection, dialect, domain |
| `acl.proto` | permissions, roles, scope |
| `contrib.proto` | **plugin features** — the one a plugin author reaches for first |
| `paging.proto`, `rpc.proto`, `mcp.proto`, `cli.proto`, `client.proto`, `error.proto`, `lock.proto` | the remaining surfaces |
| `pg/`, `mysql/`, `sqlite/` | the per-dialect escape hatches — a native column type, a dialect-only module or project setting |

**22 files.** The five under `pg/`, `mysql/` and `sqlite/` are easy to miss and
are part of the contract: `(w17.pg.column)` and its siblings are what a project
writes when it needs a type the portable vocabulary does not name.

They import nothing but each other and
`google/protobuf/{descriptor,timestamp}.proto`, which every `protoc` and `buf`
already carries. There is no further dependency to find.

## Using them

`go mod download` already puts them on disk:

```sh
VOCAB="$(go list -m -f '{{.Dir}}' github.com/wandering-compiler/sdk/go)/proto"
protoc -I "$VOCAB" -I . --go_out=. your/proto/*.proto
```

or as a `buf` input root, pointing `-I` / the module path at `$VOCAB`.

## Versioning

They ship **with** the SDK and carry its version, deliberately: the generated
half (`go/pb/w17/*.pb.go`) has always shipped here, and a source that released
separately from its own generated output would be a second version to reconcile.

A plugin says which SDK it works with in its manifest (`requires_sdk`), and which
plugin-platform contract it needs (`requires_w17` — a different number, the
contract rather than the library).

## What this is not

Not a `buf.build` module. If `deps:` resolution would help you, say so — it is
additive and publishing there from the same source costs nothing. The files are
here because they were the missing half of an artefact this repository already
publishes.
