"""Messages and services built at runtime from compiled descriptor sets.

Codegen writes no Python for a domain: it writes ``descriptors.binpb`` — the
domain's ``.proto`` files and every import, compiled into one
``FileDescriptorSet`` — so no runtime needs a proto parser. This module loads
that set (and the SDK's own, for the w17 vocabulary the runtime itself speaks:
``w17.ErrorDetail``, ``w17.Paging``, the distributed-transaction service) into
the protobuf runtime's descriptor pool and hands out message classes by full
name.

The default pool is used on purpose: the well-known types (Timestamp, Any,
Struct…) are already registered in it by the protobuf package, and a message
class built from another pool would not accept them as field values.
"""

from __future__ import annotations

import importlib.resources
from dataclasses import dataclass
from pathlib import Path

from google.protobuf import descriptor_pb2, descriptor_pool, message_factory
from google.protobuf.descriptor import (
    Descriptor,
    FileDescriptor,
    MethodDescriptor,
    ServiceDescriptor,
)
from google.protobuf.message import Message

# Imported for their side effect: each registers its well-known file in the
# default pool, so a project set that imports one finds it there instead of
# adding a second definition of the same name.
from google.protobuf import (  # noqa: F401
    any_pb2,
    duration_pb2,
    empty_pb2,
    field_mask_pb2,
    struct_pb2,
    timestamp_pb2,
    wrappers_pb2,
)

__all__ = ["Pool", "MethodInfo", "default_pool"]


@dataclass(frozen=True)
class MethodInfo:
    """One RPC as the runtime needs it."""

    service: str  # "pkg.Svc"
    name: str  # "CreateNote"
    path: str  # "/pkg.Svc/CreateNote"
    request: type[Message]
    response: type[Message]
    client_streaming: bool
    server_streaming: bool
    descriptor: MethodDescriptor


class Pool:
    """A descriptor pool with the sets the SDK and the project loaded into it."""

    def __init__(self, pool: descriptor_pool.DescriptorPool | None = None) -> None:
        self._pool = pool or descriptor_pool.Default()
        self._classes: dict[str, type[Message]] = {}
        self.files: list[str] = []

    @property
    def raw(self) -> descriptor_pool.DescriptorPool:
        return self._pool

    def add_set(self, data: bytes) -> list[str]:
        """Add a serialized FileDescriptorSet; returns the files it added.

        A file the pool already holds BY NAME is skipped: the project's set
        carries the w17 vocabulary the SDK's set carries too (``w17/error.proto``),
        and the well-known types are registered by the protobuf package. Files
        are added in dependency order, which ``protoc --include_imports``
        writes them in; the loop below does not rely on it."""
        fds = descriptor_pb2.FileDescriptorSet.FromString(data)
        pending = {f.name: f for f in fds.file if not self._has_file(f.name)}
        added: list[str] = []
        while pending:
            progressed = False
            for name, fdp in list(pending.items()):
                if any(dep in pending for dep in fdp.dependency):
                    continue
                self._pool.Add(fdp)
                added.append(name)
                del pending[name]
                progressed = True
            if not progressed:
                missing = sorted({d for f in pending.values() for d in f.dependency if d in pending})
                raise ValueError(f"descriptor set has an import cycle among {missing}")
        self.files.extend(added)
        return added

    def add_set_file(self, path: str | Path) -> list[str]:
        return self.add_set(Path(path).read_bytes())

    def _has_file(self, name: str) -> bool:
        try:
            self._pool.FindFileByName(name)
            return True
        except KeyError:
            return False

    def file(self, name: str) -> FileDescriptor:
        return self._pool.FindFileByName(name)

    def message_descriptor(self, full_name: str) -> Descriptor:
        return self._pool.FindMessageTypeByName(full_name.lstrip("."))

    def message(self, full_name: str) -> type[Message]:
        """The message class for a full name (``notes.v1.Note``)."""
        name = full_name.lstrip(".")
        cls = self._classes.get(name)
        if cls is None:
            cls = message_factory.GetMessageClass(self.message_descriptor(name))
            self._classes[name] = cls
        return cls

    def service(self, full_name: str) -> ServiceDescriptor:
        return self._pool.FindServiceByName(full_name.lstrip("."))

    def methods(self, service_full_name: str) -> list[MethodInfo]:
        sd = self.service(service_full_name)
        out = []
        for m in sd.methods:
            out.append(
                MethodInfo(
                    service=sd.full_name,
                    name=m.name,
                    path=f"/{sd.full_name}/{m.name}",
                    request=message_factory.GetMessageClass(m.input_type),
                    response=message_factory.GetMessageClass(m.output_type),
                    client_streaming=m.client_streaming,
                    server_streaming=m.server_streaming,
                    descriptor=m,
                )
            )
        return out

    def find_messages_named(self, short_name: str) -> list[str]:
        """Full names of every message called short_name in the loaded files."""
        found: list[str] = []
        for fname in self.files:
            fd = self._pool.FindFileByName(fname)
            stack = list(fd.message_types_by_name.values())
            while stack:
                d = stack.pop()
                if d.name == short_name:
                    found.append(d.full_name)
                stack.extend(d.nested_types)
        return sorted(found)


_default: Pool | None = None


def default_pool() -> Pool:
    """The process pool, with the SDK's own descriptor set loaded."""
    global _default
    if _default is None:
        p = Pool()
        data = importlib.resources.files(__package__).joinpath("sdk_descriptors.binpb").read_bytes()
        p.add_set(data)
        _default = p
    return _default
