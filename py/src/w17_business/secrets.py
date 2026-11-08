"""Secret resolution, as sdk/go/service/secret resolves it.

The chain, first hit wins:

1. the process environment;
2. ``.secrets.age`` (``W17_SECRETS_AGE_FILE``), decrypted with the age identity
   in ``W17_SECRETS_AGE_KEY`` or the file ``W17_SECRETS_AGE_KEY_FILE``
   (fallbacks ``SOPS_AGE_KEY`` / ``SOPS_AGE_KEY_FILE``);
3. ``.secrets`` (``W17_SECRETS_FILE``), plain dotenv.

``W17_SECRETS_MODE``: ``auto`` (default: the age file when a key AND the file
exist, then the plain file when it exists), ``plain`` (env + plain file only),
``age`` (strict: a key is required, the age file must decrypt). A corrupt or
undecryptable age file, or ``age`` with no key, fails the boot rather than
resolving every secret to empty.

The age file is the binary age format filippo.io/age reads (``age.Decrypt``,
X25519 identities); pyrage implements the same format.
"""

from __future__ import annotations

import os
from collections.abc import Mapping

__all__ = ["REDACTED", "Secret", "Resolver", "SecretsError", "parse_dotenv", "new_resolver"]

REDACTED = "***"


class SecretsError(Exception):
    pass


class Secret:
    """A secret value that does not print itself.

    ``repr``/``str``/format all render ``***`` (Go's ``secret.String``), so a
    config dumped to a log, a Sentry frame or an exception message carries no
    value. ``reveal()`` is the one way to read it."""

    __slots__ = ("_v",)

    def __init__(self, value: str | None) -> None:
        self._v = value

    def reveal(self) -> str:
        return self._v or ""

    def is_zero(self) -> bool:
        return not self._v

    def __repr__(self) -> str:
        return REDACTED

    __str__ = __repr__

    def __format__(self, spec: str) -> str:
        return REDACTED

    def __eq__(self, other: object) -> bool:
        return isinstance(other, Secret) and other._v == self._v

    def __hash__(self) -> int:
        return hash(("Secret", self._v))

    def __bool__(self) -> bool:
        return bool(self._v)


def parse_dotenv(s: str) -> dict[str, str]:
    """Go's parseDotenv: ``KEY=value`` lines, ``#`` comments and blank lines
    skipped, the key trimmed, the value verbatim (no quote stripping) less a
    trailing CR."""
    kv: dict[str, str] = {}
    for line in s.split("\n"):
        t = line.strip()
        if not t or t.startswith("#"):
            continue
        eq = line.find("=")
        if eq < 0:
            continue
        key = line[:eq].strip()
        if not key:
            continue
        kv[key] = line[eq + 1 :].removesuffix("\r")
    return kv


class Resolver:
    def __init__(self, sources: list[Mapping[str, str]]) -> None:
        self._sources = sources

    def lookup(self, key: str) -> tuple[str, bool]:
        for s in self._sources:
            if key in s:
                return s[key], True
        return "", False

    def get(self, key: str) -> str:
        return self.lookup(key)[0]


def _decrypt_age(path: str, key_data: str) -> dict[str, str]:
    import pyrage

    ids = []
    for line in key_data.splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        try:
            ids.append(pyrage.x25519.Identity.from_str(line))
        except Exception as e:  # noqa: BLE001
            raise SecretsError(f"secret: parse age identity: {e}") from e
    if not ids:
        raise SecretsError("secret: no age identities found in key")
    try:
        with open(path, "rb") as f:
            data = f.read()
    except OSError as e:
        raise SecretsError(f"secret: open age file {path}: {e}") from e
    try:
        plain = pyrage.decrypt(data, ids)
    except Exception as e:  # noqa: BLE001
        raise SecretsError(f"secret: decrypt {path} (wrong key?): {e}") from e
    return parse_dotenv(plain.decode("utf-8"))


def _is_file(p: str) -> bool:
    return bool(p) and os.path.isfile(p)


def new_resolver(environ: Mapping[str, str] | None = None) -> Resolver:
    env = os.environ if environ is None else environ

    def env_or(k: str, d: str) -> str:
        return env.get(k) or d

    mode = (env_or("W17_SECRETS_MODE", "auto").strip().lower()) or "auto"
    secrets_file = env_or("W17_SECRETS_FILE", ".secrets")
    age_file = env_or("W17_SECRETS_AGE_FILE", ".secrets.age")
    age_key = env.get("W17_SECRETS_AGE_KEY", "") or env.get("SOPS_AGE_KEY", "")
    age_key_file = env.get("W17_SECRETS_AGE_KEY_FILE", "") or env.get("SOPS_AGE_KEY_FILE", "")

    key_data, have_key = "", False
    if age_key.strip():
        key_data, have_key = age_key, True
    elif age_key_file.strip():
        try:
            with open(age_key_file, encoding="utf-8") as f:
                key_data, have_key = f.read(), True
        except OSError as e:
            raise SecretsError(f"secret: read age key file {age_key_file}: {e}") from e

    sources: list[Mapping[str, str]] = [env]

    def plain() -> None:
        if _is_file(secrets_file):
            try:
                with open(secrets_file, encoding="utf-8") as f:
                    sources.append(parse_dotenv(f.read()))
            except OSError as e:
                raise SecretsError(f"secret: read plain secrets file {secrets_file!r}: {e}") from e

    if mode == "plain":
        plain()
    elif mode == "age":
        if not have_key:
            raise SecretsError("secret: mode=age but no age key (set W17_SECRETS_AGE_KEY or W17_SECRETS_AGE_KEY_FILE)")
        sources.append(_decrypt_age(age_file, key_data))
    elif mode == "auto":
        if have_key and _is_file(age_file):
            sources.append(_decrypt_age(age_file, key_data))
        plain()
    else:
        raise SecretsError(f"secret: unknown W17_SECRETS_MODE {env.get('W17_SECRETS_MODE')!r} (auto|plain|age)")
    return Resolver(sources)
