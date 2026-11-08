"""w17 business SDK for Python (asyncio).

Serves a domain's business tier from what codegen wrote — ``descriptors.binpb``
and ``w17-business.json`` — with sdk/go's semantics: header forwarding,
distributed transactions, events after commit, ``w17.ErrorDetail`` errors.

The project provides one function::

    def register_business(cfg, registry, clients):
        registry.register("notes.v1.NoteService", NoteService(cfg, registry, clients))

and the handler methods, named exactly as the RPCs, each taking the context
first: ``async def CreateNote(self, ctx, req)``. Pass ``ctx`` on to every call.
"""

from . import distx, events, paging, principal, txscope
from .client import ServiceClient
from .config import Config, ConfigError
from .context import Context, Metadata, background
from .distx import on_commit
from .errors import Status, UserError, error_detail, for_user
from .runtime import App, Clients
from .secrets import Secret
from .server import Registry, RegistrationError, Served

__all__ = [
    "App",
    "Clients",
    "Config",
    "ConfigError",
    "Context",
    "Metadata",
    "Registry",
    "RegistrationError",
    "Secret",
    "Served",
    "ServiceClient",
    "Status",
    "UserError",
    "background",
    "distx",
    "error_detail",
    "events",
    "for_user",
    "on_commit",
    "paging",
    "principal",
    "txscope",
]
