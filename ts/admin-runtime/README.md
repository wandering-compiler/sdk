# @w17/admin-runtime

The React (Mantine) runtime behind a generated admin SPA. The scaffold w17
emits for an admin bundle imports `bootstrap` from it and hands it the
embedded admin spec; the list, detail, create, inline and overview pages are
all rendered from that spec, so a project writes no admin UI code.

When the spec says the admin binds sessions to a device (`auth.dpop`), every
request is signed with DPoP. The package carries its own copy of
[`@w17/dpop`](../dpop)'s browser sources in `src/dpop/` (`make sync-admin-dpop`,
checked by `make check-admin-dpop`), because the platform ships the admin
runtime alone.

`npm test` runs the tests (vitest).
