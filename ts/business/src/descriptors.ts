/**
 * Messages and services built at runtime from compiled descriptor sets.
 *
 * Codegen writes no TypeScript for a domain: it writes `descriptors.binpb` —
 * the domain's `.proto` files and every import, compiled into one
 * `FileDescriptorSet` — so no runtime needs a proto parser. This module loads
 * that set (and the SDK's own, for the w17 vocabulary the runtime itself
 * speaks: `w17.ErrorDetail`, `w17.Paging`, the distributed-transaction
 * service) into one protobuf-es registry and hands out message schemas and
 * constructors by full name.
 *
 * Messages are protobuf-es v2 messages: plain objects with a `$typeName`,
 * fields under their camelCase `localName` (`user_id` → `userId`), 64-bit
 * integers as `bigint`, a oneof as `{ case, value }`.
 */

import { readFileSync } from "node:fs";
import {
  create,
  createFileRegistry,
  fromBinary,
  toBinary,
  type DescFile,
  type DescMessage,
  type DescMethod,
  type DescService,
  type FileRegistry,
  type Message,
  type MessageInitShape,
} from "@bufbuild/protobuf";
import {
  FileDescriptorSetSchema,
  type FileDescriptorProto,
  file_google_protobuf_any,
  file_google_protobuf_descriptor,
  file_google_protobuf_duration,
  file_google_protobuf_empty,
  file_google_protobuf_field_mask,
  file_google_protobuf_struct,
  file_google_protobuf_timestamp,
  file_google_protobuf_wrappers,
} from "@bufbuild/protobuf/wkt";

/** A dynamic message: its fields are known only at run time. */
 
export type AnyMessage = Message & Record<string, any>;

/** A message "class": call it to create one, with its schema beside it. */
export interface MessageType {
  (init?: Record<string, unknown>): AnyMessage;
  readonly schema: DescMessage;
  readonly typeName: string;
  fromBinary(bytes: Uint8Array): AnyMessage;
  toBinary(msg: Message): Uint8Array;
}

/** One RPC as the runtime needs it. */
export interface MethodInfo {
  /** "pkg.Svc" */
  readonly service: string;
  /** "CreateNote" */
  readonly name: string;
  /** "/pkg.Svc/CreateNote" */
  readonly path: string;
  readonly input: DescMessage;
  readonly output: DescMessage;
  readonly clientStreaming: boolean;
  readonly serverStreaming: boolean;
  readonly descriptor: DescMethod;
}

/** The well-known files every set may import, registered up front. */
const WKT: readonly DescFile[] = [
  file_google_protobuf_any,
  file_google_protobuf_descriptor,
  file_google_protobuf_duration,
  file_google_protobuf_empty,
  file_google_protobuf_field_mask,
  file_google_protobuf_struct,
  file_google_protobuf_timestamp,
  file_google_protobuf_wrappers,
];

export class KeyError extends Error {
  override name = "KeyError";
}

/** A registry with the sets the SDK and the project loaded into it. */
export class Pool {
  #files = new Map<string, DescFile>();
  #registry: FileRegistry;
  #types = new Map<string, MessageType>();
  /** The files added from descriptor sets, in the order they were added. */
  readonly files: string[] = [];

  constructor() {
    for (const f of WKT) {
      this.#files.set(f.proto.name, f);
    }
    this.#registry = createFileRegistry(...WKT.map((f) => createFileRegistry(fileProto(f), () => undefined)));
  }

  get registry(): FileRegistry {
    return this.#registry;
  }

  /**
   * Add a serialized FileDescriptorSet; returns the files it added.
   *
   * A file the pool already holds BY NAME is skipped: the project's set
   * carries the w17 vocabulary the SDK's set carries too (`w17/error.proto`),
   * and the well-known types are built in. Files are added in dependency
   * order, which `protoc --include_imports` writes them in; the loop below
   * does not rely on it.
   */
  addSet(data: Uint8Array): string[] {
    const fds = fromBinary(FileDescriptorSetSchema, data);
    const pending = new Map<string, FileDescriptorProto>();
    for (const f of fds.file) if (!this.#files.has(f.name)) pending.set(f.name, f);
    const added: string[] = [];
    const regs: FileRegistry[] = [this.#registry];
    while (pending.size) {
      let progressed = false;
      for (const [name, fdp] of [...pending]) {
        if (fdp.dependency.some((d) => pending.has(d))) continue;
        const reg = createFileRegistry(fdp, (dep) => this.#files.get(dep));
        const file = reg.getFile(name);
        if (file === undefined) throw new Error(`descriptor set: ${name} did not register`);
        this.#files.set(name, file);
        regs.push(reg);
        added.push(name);
        pending.delete(name);
        progressed = true;
      }
      if (!progressed) {
        const missing = new Set<string>();
        for (const f of pending.values()) for (const d of f.dependency) if (pending.has(d)) missing.add(d);
        throw new Error(`descriptor set has an import cycle among ${[...missing].sort().join(", ")}`);
      }
    }
    this.#registry = createFileRegistry(...regs);
    this.files.push(...added);
    return added;
  }

  addSetFile(path: string): string[] {
    return this.addSet(readFileSync(path));
  }

  file(name: string): DescFile {
    const f = this.#files.get(name);
    if (f === undefined) throw new KeyError(`no file ${JSON.stringify(name)} in the descriptor pool`);
    return f;
  }

  /** The schema of a message by full name (`notes.v1.Note`). */
  schema(fullName: string): DescMessage {
    const name = fullName.replace(/^\./, "");
    const d = this.#registry.getMessage(name);
    if (d === undefined) throw new KeyError(`no message ${JSON.stringify(name)} in the descriptor pool`);
    return d;
  }

  /** The message type for a full name: `pool.message("notes.v1.Note")({ text: "x" })`. */
  message(fullName: string): MessageType {
    const name = fullName.replace(/^\./, "");
    let t = this.#types.get(name);
    if (t === undefined) {
      const schema = this.schema(name);
      const fn = ((init?: Record<string, unknown>) =>
        create(schema, init as MessageInitShape<DescMessage>) as AnyMessage) as MessageType;
      Object.defineProperties(fn, {
        schema: { value: schema },
        typeName: { value: schema.typeName },
        fromBinary: { value: (b: Uint8Array) => fromBinary(schema, b) as AnyMessage },
        toBinary: { value: (m: Message) => toBinary(schema, m) },
        name: { value: schema.name },
      });
      t = fn;
      this.#types.set(name, t);
    }
    return t;
  }

  service(fullName: string): DescService {
    const name = fullName.replace(/^\./, "");
    const d = this.#registry.getService(name);
    if (d === undefined) throw new KeyError(`no service ${JSON.stringify(name)} in the descriptor pool`);
    return d;
  }

  /** Every service whose short name is `short`, by full name. */
  servicesNamed(short: string): string[] {
    const out = new Set<string>();
    for (const f of this.files) for (const s of this.file(f).services) if (s.name === short) out.add(s.typeName);
    return [...out].sort();
  }

  methods(serviceFullName: string): MethodInfo[] {
    const sd = this.service(serviceFullName);
    return sd.methods.map((m) => ({
      service: sd.typeName,
      name: m.name,
      path: `/${sd.typeName}/${m.name}`,
      input: m.input,
      output: m.output,
      clientStreaming: m.methodKind === "client_streaming" || m.methodKind === "bidi_streaming",
      serverStreaming: m.methodKind === "server_streaming" || m.methodKind === "bidi_streaming",
      descriptor: m,
    }));
  }

  /** Full names of every message called shortName in the loaded files. */
  findMessagesNamed(shortName: string): string[] {
    const found: string[] = [];
    for (const fname of this.files) {
      const stack: DescMessage[] = [...this.file(fname).messages];
      while (stack.length) {
        const d = stack.pop()!;
        if (d.name === shortName) found.push(d.typeName);
        stack.push(...d.nestedMessages);
      }
    }
    return found.sort();
  }

  /** The schema a message was created from, found by its `$typeName`. */
  schemaOf(msg: Message): DescMessage {
    return this.schema(msg.$typeName);
  }
}

function fileProto(f: DescFile): FileDescriptorProto {
  return f.proto;
}

let defaultPool: Pool | undefined;

/** The process pool, with the SDK's own descriptor set loaded. */
export function getDefaultPool(): Pool {
  if (defaultPool === undefined) {
    const p = new Pool();
    p.addSet(readFileSync(new URL("./sdk_descriptors.binpb", import.meta.url)));
    defaultPool = p;
  }
  return defaultPool;
}
