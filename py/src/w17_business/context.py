"""The context a handler is given and passes on.

Go's rule — "the context you were given is the one you pass on" — is what keeps
the caller's identity, deadline and transaction attached to every call a handler
makes. Python has no ``context.Context``, and the ambient substitutes
(``contextvars``) lose their value in work spawned outside the request's task
without anyone noticing. So the SDK makes the context an explicit, immutable
value: every handler receives one, every client call takes one first, and every
helper that changes it (``paging.forward``, ``principal.act_as``, ``distx.begin``)
returns a NEW one rather than mutating the one it was given — exactly like a Go
``context.WithValue``.

A context carries:

- the INCOMING metadata of the request being served (what the gateway stamped);
- the OUTGOING metadata the next call will send (empty until something sets it);
- the deadline, as an absolute ``time.monotonic()`` instant (``None`` = none);
- a cancellation flag (``cancel()``), the analogue of a Go cancel func;
- values keyed by private objects (the open transaction lives there).
"""

from __future__ import annotations

import time
from collections.abc import Iterable, Mapping
from typing import Any

__all__ = ["Context", "Metadata", "background"]


class Metadata(Mapping[str, tuple[str, ...]]):
    """gRPC metadata: lower-case keys, each with one or more string values.

    Immutable, like a Go ``metadata.MD`` handed between contexts after a
    ``Copy()``: a helper that changes metadata builds a new value, so two
    contexts never share a list one of them could rewrite.

    Keys are lower-cased on the way in, because HTTP/2 and grpc-go both
    normalise them; comparing a mixed-case constant against a wire key would
    otherwise silently miss.
    """

    __slots__ = ("_d",)

    def __init__(self, items: Mapping[str, Any] | Iterable[tuple[str, Any]] | None = None) -> None:
        d: dict[str, list[str]] = {}
        if items is None:
            pass
        elif isinstance(items, Mapping):
            for k, v in items.items():
                vals = [v] if isinstance(v, (str, bytes)) else list(v)
                d.setdefault(k.lower(), []).extend(_as_str(x) for x in vals)
        else:
            for k, v in items:
                d.setdefault(k.lower(), []).append(_as_str(v))
        self._d = {k: tuple(v) for k, v in d.items() if v}

    def __getitem__(self, key: str) -> tuple[str, ...]:
        return self._d[key.lower()]

    def __iter__(self):
        return iter(self._d)

    def __len__(self) -> int:
        return len(self._d)

    def get_all(self, key: str) -> tuple[str, ...]:
        """Every value of key, ``()`` when absent (Go's ``md.Get``)."""
        return self._d.get(key.lower(), ())

    def first(self, key: str) -> str | None:
        vals = self._d.get(key.lower())
        return vals[0] if vals else None

    def with_set(self, key: str, *values: str) -> Metadata:
        """A copy with key replaced by values (Go's ``md.Set``); no values deletes it."""
        d = dict(self._d)
        if values:
            d[key.lower()] = tuple(values)
        else:
            d.pop(key.lower(), None)
        return Metadata._of(d)

    def without(self, pred) -> Metadata:
        """A copy without the keys pred(key) answers true for."""
        return Metadata._of({k: v for k, v in self._d.items() if not pred(k)})

    def merged(self, other: Mapping[str, Iterable[str]]) -> Metadata:
        """A copy with other's keys REPLACING this one's."""
        d = dict(self._d)
        for k, v in other.items():
            d[k.lower()] = tuple(v)
        return Metadata._of(d)

    def pairs(self) -> list[tuple[str, str]]:
        """Flattened (key, value) pairs, for a grpclib call."""
        return [(k, v) for k, vals in self._d.items() for v in vals]

    @classmethod
    def _of(cls, d: dict[str, tuple[str, ...]]) -> Metadata:
        md = cls.__new__(cls)
        md._d = {k: v for k, v in d.items() if v}
        return md

    def __repr__(self) -> str:
        return f"Metadata({self._d!r})"

    def __eq__(self, other: object) -> bool:
        if isinstance(other, Metadata):
            return self._d == other._d
        return NotImplemented

    __hash__ = None  # type: ignore[assignment]


def _as_str(v: Any) -> str:
    if isinstance(v, bytes):
        # grpclib decodes `-bin` keys to bytes; the w17 keys are all text, so
        # anything binary is carried as latin-1 rather than dropped.
        return v.decode("latin-1")
    return str(v)


class _Cancel:
    """A cancellation flag shared by a context and the contexts derived from it."""

    __slots__ = ("cancelled", "parent")

    def __init__(self, parent: _Cancel | None = None) -> None:
        self.cancelled = False
        self.parent = parent

    def is_set(self) -> bool:
        c: _Cancel | None = self
        while c is not None:
            if c.cancelled:
                return True
            c = c.parent
        return False


class Context:
    """An immutable request context. See the module doc."""

    __slots__ = ("incoming", "outgoing", "deadline", "_cancel", "_values", "method")

    def __init__(
        self,
        incoming: Metadata | Mapping[str, Any] | None = None,
        outgoing: Metadata | Mapping[str, Any] | None = None,
        deadline: float | None = None,
        *,
        method: str = "",
        _cancel: _Cancel | None = None,
        _values: dict[object, Any] | None = None,
    ) -> None:
        self.incoming = incoming if isinstance(incoming, Metadata) else Metadata(incoming)
        self.outgoing = outgoing if isinstance(outgoing, Metadata) else Metadata(outgoing)
        self.deadline = deadline
        # The full gRPC method being served ("/pkg.Svc/Method"), "" outside a
        # handler. Error helpers use it as Go's `method` prefix.
        self.method = method
        self._cancel = _cancel or _Cancel()
        self._values = _values or {}

    # --- derivation -----------------------------------------------------------

    def _replace(self, **kw: Any) -> Context:
        c = Context.__new__(Context)
        c.incoming = kw.get("incoming", self.incoming)
        c.outgoing = kw.get("outgoing", self.outgoing)
        c.deadline = kw.get("deadline", self.deadline)
        c.method = kw.get("method", self.method)
        c._cancel = kw.get("_cancel", self._cancel)
        c._values = kw.get("_values", self._values)
        return c

    def with_incoming(self, md: Metadata | Mapping[str, Any] | None) -> Context:
        return self._replace(incoming=md if isinstance(md, Metadata) else Metadata(md))

    def with_outgoing(self, md: Metadata | Mapping[str, Any] | None) -> Context:
        return self._replace(outgoing=md if isinstance(md, Metadata) else Metadata(md))

    def with_value(self, key: object, value: Any) -> Context:
        v = dict(self._values)
        v[key] = value
        return self._replace(_values=v)

    def value(self, key: object, default: Any = None) -> Any:
        return self._values.get(key, default)

    def with_timeout(self, seconds: float) -> Context:
        """A child whose deadline is the earlier of this one's and now+seconds."""
        d = time.monotonic() + seconds
        if self.deadline is not None and self.deadline < d:
            d = self.deadline
        return self._replace(deadline=d, _cancel=_Cancel(self._cancel))

    def with_cancel(self) -> Context:
        """A child that ``cancel()`` ends without ending this one."""
        return self._replace(_cancel=_Cancel(self._cancel))

    def without_cancel(self) -> Context:
        """Go's ``context.WithoutCancel``: same metadata and values, no deadline,
        never done. For work that must outlive the request — a rollback, a
        commit-outcome probe, an event published after commit."""
        return self._replace(deadline=None, _cancel=_Cancel())

    # --- state ----------------------------------------------------------------

    def cancel(self) -> None:
        self._cancel.cancelled = True

    def time_remaining(self) -> float | None:
        if self.deadline is None:
            return None
        return self.deadline - time.monotonic()

    def done(self) -> bool:
        """True once cancelled or past the deadline (Go's ``ctx.Err() != nil``)."""
        if self._cancel.is_set():
            return True
        return self.deadline is not None and time.monotonic() >= self.deadline

    def err(self) -> str | None:
        """Why the context is done — "context canceled" / "context deadline
        exceeded", Go's spellings — or None."""
        if self._cancel.is_set():
            return "context canceled"
        if self.deadline is not None and time.monotonic() >= self.deadline:
            return "context deadline exceeded"
        return None

    def __repr__(self) -> str:
        return f"Context(method={self.method!r}, incoming={list(self.incoming)}, outgoing={list(self.outgoing)}, deadline={self.deadline})"


def background() -> Context:
    """An empty context: no metadata, no deadline (Go's ``context.Background()``)."""
    return Context()
