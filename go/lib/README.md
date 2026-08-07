# sdk/go/lib

Runtime helper libraries the wandering-compiler **emits calls into** —
they run inside a generated service binary but are **not hand-imported**.
A developer writing a custom business layer never imports these directly;
that surface is `sdk/go/service` (`runtime.Serve` + `bootstrap`). These are
here purely so the compiler's generated code has a public, stable home for
its runtime dependencies (eventbus dispatch, REST/JSON marshaling, i18n,
ACL enforcement, KV storage, DQL binding, gRPC client/server helpers, …).

Named by *nature* (libraries), unlike the audience-named `core` / `service`
/ `tooling` tiers — because their only consumer is the compiler's emitted
code, which has no human role to name.
