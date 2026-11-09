/**
 * `grpc.health.v1.Health` (Check and Watch), served beside the business
 * services, and the client half the `health` subcommand probes it with.
 *
 * The two messages have one field each, so they are encoded here by hand
 * rather than carried in a descriptor set.
 */

import {
  Client,
  Metadata as GrpcMetadata,
  type ChannelCredentials,
  type ChannelOptions,
  type MethodDefinition,
  type Server,
  type ServerUnaryCall,
  type ServerWritableStream,
  type sendUnaryData,
} from "@grpc/grpc-js";
import { BinaryReader, BinaryWriter, WireType } from "@bufbuild/protobuf/wire";
import { Status } from "./errors.js";

export const SERVICE = "grpc.health.v1.Health";

export const ServingStatus = {
  UNKNOWN: 0,
  SERVING: 1,
  NOT_SERVING: 2,
  SERVICE_UNKNOWN: 3,
} as const;

const statusNames = ["UNKNOWN", "SERVING", "NOT_SERVING", "SERVICE_UNKNOWN"];
export function servingStatusName(s: number): string {
  return statusNames[s] ?? String(s);
}

/** HealthCheckRequest{service} → bytes. */
export function encodeRequest(service: string): Buffer {
  const w = new BinaryWriter();
  if (service) w.tag(1, WireType.LengthDelimited).string(service);
  return Buffer.from(w.finish());
}

export function decodeRequest(b: Uint8Array): string {
  const r = new BinaryReader(b);
  let service = "";
  while (r.pos < r.len) {
    const [no, wt] = r.tag();
    if (no === 1 && wt === WireType.LengthDelimited) service = r.string();
    else r.skip(wt);
  }
  return service;
}

export function encodeResponse(status: number): Buffer {
  const w = new BinaryWriter();
  if (status) w.tag(1, WireType.Varint).int32(status);
  return Buffer.from(w.finish());
}

export function decodeResponse(b: Uint8Array): number {
  const r = new BinaryReader(b);
  let status = 0;
  while (r.pos < r.len) {
    const [no, wt] = r.tag();
    if (no === 1 && wt === WireType.Varint) status = r.int32();
    else r.skip(wt);
  }
  return status;
}

const raw = (b: Buffer) => b;

function def(name: string, responseStream: boolean): MethodDefinition<Buffer, Buffer> {
  return {
    path: `/${SERVICE}/${name}`,
    requestStream: false,
    responseStream,
    requestSerialize: raw,
    requestDeserialize: raw,
    responseSerialize: raw,
    responseDeserialize: raw,
    originalName: name,
  };
}

/** The overall status ("") and nothing else, as the Python and Go tiers register it. */
export class HealthService {
  readonly #status = new Map<string, number>([["", ServingStatus.NOT_SERVING]]);
  readonly #watchers = new Set<(service: string) => void>();

  setStatus(service: string, status: number): void {
    this.#status.set(service, status);
    for (const w of [...this.#watchers]) w(service);
  }

  /** The overall status: serving or not. */
  set(serving: boolean): void {
    this.setStatus("", serving ? ServingStatus.SERVING : ServingStatus.NOT_SERVING);
  }

  addTo(server: Server): void {
    server.addService(
      { Check: def("Check", false), Watch: def("Watch", true) },
      {
        Check: (call: ServerUnaryCall<Buffer, Buffer>, cb: sendUnaryData<Buffer>) => {
          const service = decodeRequest(call.request);
          const s = this.#status.get(service);
          if (s === undefined) {
            cb({ code: Status.NOT_FOUND, details: "unknown service" }, null);
            return;
          }
          cb(null, encodeResponse(s));
        },
        Watch: (call: ServerWritableStream<Buffer, Buffer>) => {
          const service = decodeRequest(call.request);
          let last = -1;
          const send = () => {
            const s = this.#status.get(service) ?? ServingStatus.SERVICE_UNKNOWN;
            if (s !== last) {
              last = s;
              call.write(encodeResponse(s));
            }
          };
          const watcher = (changed: string) => {
            if (changed === service) send();
          };
          this.#watchers.add(watcher);
          call.on("cancelled", () => {
            this.#watchers.delete(watcher);
            call.end();
          });
          send();
        },
      },
    );
  }
}

/** Ask a listener for its overall status; resolves to the ServingStatus. */
export function check(
  target: string,
  creds: ChannelCredentials,
  timeoutMs: number,
  options: ChannelOptions = {},
): Promise<number> {
  const client = new Client(target, creds, options);
  return new Promise<number>((resolve, reject) => {
    client.makeUnaryRequest(
      `/${SERVICE}/Check`,
      raw,
      (b: Buffer) => b,
      encodeRequest(""),
      new GrpcMetadata(),
      { deadline: new Date(Date.now() + timeoutMs) },
      (err, resp) => {
        if (err) reject(new Error(`${Status[err.code] ?? err.code}: ${err.details}`));
        else resolve(decodeResponse(resp ?? Buffer.alloc(0)));
      },
    );
  }).finally(() => client.close());
}
