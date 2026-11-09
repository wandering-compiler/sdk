/**
 * gRPC server reflection (`grpc.reflection.v1` and `v1alpha`), registered
 * only when `<PREFIX>_GRPC_REFLECTION` is `1`/`true`, as the Go tier does.
 *
 * Answers from the SDK's descriptor pool: list_services,
 * file_by_filename and file_containing_symbol (with every file it imports);
 * extension queries answer NOT_FOUND (the w17 vocabulary's extensions are
 * options, never message extensions a client asks for). The messages are
 * encoded here by hand rather than carried in a descriptor set.
 */

import type { MethodDefinition, Server, ServerDuplexStream } from "@grpc/grpc-js";
import { toBinary, type DescFile } from "@bufbuild/protobuf";
import { BinaryReader, BinaryWriter, WireType } from "@bufbuild/protobuf/wire";
import { FileDescriptorProtoSchema } from "@bufbuild/protobuf/wkt";
import type { Pool } from "./descriptors.js";
import { Status } from "./errors.js";

export const SERVICES = ["grpc.reflection.v1.ServerReflection", "grpc.reflection.v1alpha.ServerReflection"];

interface Request {
  host: string;
  raw: Uint8Array;
  kind: "file_by_filename" | "file_containing_symbol" | "file_containing_extension" | "all_extension_numbers_of_type" | "list_services" | "";
  arg: string;
}

function decodeRequest(b: Uint8Array): Request {
  const r = new BinaryReader(b);
  const req: Request = { host: "", raw: b, kind: "", arg: "" };
  while (r.pos < r.len) {
    const [no, wt] = r.tag();
    if (wt !== WireType.LengthDelimited) {
      r.skip(wt);
      continue;
    }
    switch (no) {
      case 1:
        req.host = r.string();
        break;
      case 3:
        req.kind = "file_by_filename";
        req.arg = r.string();
        break;
      case 4:
        req.kind = "file_containing_symbol";
        req.arg = r.string();
        break;
      case 5:
        req.kind = "file_containing_extension";
        r.bytes();
        break;
      case 6:
        req.kind = "all_extension_numbers_of_type";
        req.arg = r.string();
        break;
      case 7:
        req.kind = "list_services";
        req.arg = r.string();
        break;
      default:
        r.skip(wt);
    }
  }
  return req;
}

function response(req: Request, body: (w: BinaryWriter) => void): Buffer {
  const w = new BinaryWriter();
  if (req.host) w.tag(1, WireType.LengthDelimited).string(req.host);
  w.tag(2, WireType.LengthDelimited).bytes(req.raw);
  body(w);
  return Buffer.from(w.finish());
}

function errorResponse(req: Request, code: number, message: string): Buffer {
  return response(req, (w) => {
    w.tag(7, WireType.LengthDelimited).fork();
    if (code) w.tag(1, WireType.Varint).int32(code);
    if (message) w.tag(2, WireType.LengthDelimited).string(message);
    w.join();
  });
}

/** The file and everything it imports, each once. */
function withDeps(file: DescFile): DescFile[] {
  const out: DescFile[] = [];
  const seen = new Set<string>();
  const visit = (f: DescFile) => {
    if (seen.has(f.proto.name)) return;
    seen.add(f.proto.name);
    out.push(f);
    for (const d of f.dependencies) visit(d);
  };
  visit(file);
  return out;
}

function filesResponse(req: Request, file: DescFile): Buffer {
  return response(req, (w) => {
    w.tag(4, WireType.LengthDelimited).fork();
    for (const f of withDeps(file)) w.tag(1, WireType.LengthDelimited).bytes(toBinary(FileDescriptorProtoSchema, f.proto));
    w.join();
  });
}

function fileOfSymbol(pool: Pool, symbol: string): DescFile | undefined {
  const reg = pool.registry;
  const d = reg.get(symbol);
  if (d !== undefined) return d.file;
  // A method or a field: "pkg.Svc.Method", "pkg.Msg.field".
  const parent = symbol.slice(0, symbol.lastIndexOf("."));
  return parent ? reg.get(parent)?.file : undefined;
}

export function answer(pool: Pool, services: () => string[], b: Uint8Array): Buffer {
  const req = decodeRequest(b);
  switch (req.kind) {
    case "list_services":
      return response(req, (w) => {
        w.tag(6, WireType.LengthDelimited).fork();
        for (const name of services()) {
          w.tag(1, WireType.LengthDelimited).fork().tag(1, WireType.LengthDelimited).string(name).join();
        }
        w.join();
      });
    case "file_by_filename": {
      let f: DescFile | undefined;
      try {
        f = pool.file(req.arg);
      } catch {
        f = pool.registry.getFile(req.arg);
      }
      return f ? filesResponse(req, f) : errorResponse(req, Status.NOT_FOUND, `file not found: ${req.arg}`);
    }
    case "file_containing_symbol": {
      const f = fileOfSymbol(pool, req.arg);
      return f ? filesResponse(req, f) : errorResponse(req, Status.NOT_FOUND, `symbol not found: ${req.arg}`);
    }
    case "file_containing_extension":
    case "all_extension_numbers_of_type":
      return errorResponse(req, Status.NOT_FOUND, "extensions are not served");
    default:
      return errorResponse(req, Status.INVALID_ARGUMENT, "unknown reflection request");
  }
}

const raw = (b: Buffer) => b;

/** Register both reflection services; `services` lists what the server serves. */
export function addTo(server: Server, pool: Pool, services: () => string[]): void {
  for (const svc of SERVICES) {
    const d: MethodDefinition<Buffer, Buffer> = {
      path: `/${svc}/ServerReflectionInfo`,
      requestStream: true,
      responseStream: true,
      requestSerialize: raw,
      requestDeserialize: raw,
      responseSerialize: raw,
      responseDeserialize: raw,
      originalName: "ServerReflectionInfo",
    };
    server.addService(
      { ServerReflectionInfo: d },
      {
        ServerReflectionInfo: (call: ServerDuplexStream<Buffer, Buffer>) => {
          // These listeners run on grpc-js's emitter: a throw from one is an
          // uncaught exception that ends the process. A request that does
          // not decode ends the call instead, as Go's codec refuses it.
          call.on("data", (b: Buffer) => {
            let out: Buffer;
            try {
              out = answer(pool, services, b);
            } catch (e) {
              call.removeAllListeners("data");
              call.emit("error", { code: Status.INVALID_ARGUMENT, details: `grpc: error unmarshalling request: ${e instanceof Error ? e.message : String(e)}` });
              return;
            }
            call.write(out);
          });
          call.on("end", () => call.end());
        },
      },
    );
  }
}
