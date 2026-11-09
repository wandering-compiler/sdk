// The module the manifest's `register` names: the tier's entry point.

import type { Clients, Config, Registry } from "@w17/business";
import { NoteService } from "./note_service.ts";

export function registerBusiness(cfg: Config, registry: Registry, clients: Clients): void {
  registry.register("conformance.v1.NoteService", new NoteService(cfg, registry, clients));
}
