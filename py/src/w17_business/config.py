"""The tier's configuration: every key the manifest declares, read at boot.

Go generates a typed ``EnvConfig`` struct; the dynamic SDKs get the same
values behind typed getters instead. The rules are Go's ``envStr`` /
``envBool`` / ``envInt``:

- an unset (or empty) variable takes the declared default;
- a value that does not parse as the declared type stops the boot, naming the
  variable — a typo in a production env must not run with a default;
- ``bool`` accepts ``true``, ``false``, ``1``, ``0`` and nothing else;
- ``int`` is a base-10 integer (Go's ``strconv.Atoi``);
- a ``secret`` key is resolved through the secrets chain (env, ``.secrets.age``,
  ``.secrets``) and comes back as a ``Secret`` that never prints its value.

Beyond Go: a key declared ``required`` with no value and no default stops the
boot too, naming the key and its variable.
"""

from __future__ import annotations

import os
import re
from collections.abc import Mapping

from .manifest import ConfigKey
from .secrets import Resolver, Secret, new_resolver

__all__ = ["Config", "ConfigError"]

_INT = re.compile(r"[+-]?[0-9]+")
_INT64_MIN, _INT64_MAX = -(2**63), 2**63 - 1


class ConfigError(Exception):
    pass


def _parse(k: ConfigKey, raw: str) -> object:
    if k.type in ("", "string"):
        return raw
    if k.type == "bool":
        if raw in ("true", "1"):
            return True
        if raw in ("false", "0"):
            return False
        raise ConfigError(f"env {k.env}: invalid bool {raw!r} (want true, false, 1 or 0)")
    if k.type == "int":
        if not _INT.fullmatch(raw):
            raise ConfigError(f"env {k.env}: invalid int {raw!r}")
        # Go's strconv.Atoi on a 64-bit platform: a value past int64 is refused.
        # (The length check first: int() itself refuses a few thousand digits.)
        digits = raw.lstrip("+-").lstrip("0")
        n = 0 if len(digits) > 19 else int(digits or "0") * (-1 if raw.startswith("-") else 1)
        if len(digits) > 19 or not _INT64_MIN <= n <= _INT64_MAX:
            raise ConfigError(f"env {k.env}: invalid int {raw!r}: value out of range (64-bit)")
        return n
    raise ConfigError(f"config key {k.name!r}: unknown type {k.type!r} (string, bool, int)")


class Config:
    """The declared configuration, by key name.

    ``cfg.str("greeting")``, ``cfg.bool("verbose")``, ``cfg.int("limit")``,
    ``cfg.secret("api_token").reveal()``; ``cfg["greeting"]`` returns the typed
    value. Reading an undeclared key raises KeyError — a typo must not read as
    "unset"."""

    def __init__(self, keys: tuple[ConfigKey, ...], values: dict[str, object]) -> None:
        self._keys = {k.name: k for k in keys}
        self._values = values

    @classmethod
    def load(
        cls,
        keys: tuple[ConfigKey, ...],
        environ: Mapping[str, str] | None = None,
        resolver: Resolver | None = None,
    ) -> Config:
        env = os.environ if environ is None else environ
        values: dict[str, object] = {}
        if any(k.secret for k in keys) and resolver is None:
            try:
                resolver = new_resolver(env)
            except Exception as e:
                raise ConfigError(f"secrets: {e}") from e
        for k in keys:
            if k.secret:
                raw = resolver.get(k.env) if resolver else ""
                if not raw and k.required:
                    raise ConfigError(f"config key {k.name!r}: {k.env} is required")
                values[k.name] = Secret(raw or None)
                continue
            raw = env.get(k.env, "")
            if raw == "":
                raw = k.default
            if raw == "":
                if k.required:
                    raise ConfigError(f"config key {k.name!r}: {k.env} is required")
                values[k.name] = {"bool": False, "int": 0}.get(k.type, "")
                continue
            values[k.name] = _parse(k, raw)
        return cls(keys, values)

    def _get(self, name: str, kind: str) -> object:
        k = self._keys.get(name)
        if k is None:
            raise KeyError(f"config: no key {name!r} is declared (declared: {sorted(self._keys)})")
        want = "secret" if k.secret else (k.type or "string")
        if want != kind:
            raise TypeError(f"config: key {name!r} is a {want}, not a {kind}")
        return self._values[name]

    def str(self, name: str) -> str:
        return self._get(name, "string")  # type: ignore[return-value]

    def bool(self, name: str) -> bool:
        return self._get(name, "bool")  # type: ignore[return-value]

    def int(self, name: str) -> int:
        return self._get(name, "int")  # type: ignore[return-value]

    def secret(self, name: str) -> Secret:
        return self._get(name, "secret")  # type: ignore[return-value]

    def __getitem__(self, name: str) -> object:
        if name not in self._keys:
            raise KeyError(f"config: no key {name!r} is declared")
        return self._values[name]

    def __contains__(self, name: object) -> bool:
        return name in self._keys

    def keys(self):
        return self._keys.keys()

    def secret_env_names(self) -> list[str]:
        return [k.env for k in self._keys.values() if k.secret]

    def __repr__(self) -> str:
        # Secrets render as *** through Secret.__repr__.
        return f"Config({self._values!r})"
