"""The module the manifest's ``register`` names: the tier's entry point."""

from . import NoteService


def register_business(cfg, registry, clients):
    registry.register("conformance.v1.NoteService", NoteService(cfg, registry, clients))
