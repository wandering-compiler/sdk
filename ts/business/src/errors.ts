/**
 * Errors that say one thing to an operator and another to a person.
 *
 * The TypeScript twin of `grpcerr.ForUser` (sdk/go/core/grpcerr): the gRPC
 * status carries the OPERATOR's copy, prefixed with the method
 * (`Svc.Method: <dev>`), and a `w17.ErrorDetail{code, message, field}` carries
 * the caller's sentence, which the gateway renders. Encoded as Go encodes it:
 * a `google.rpc.Status` in `grpc-status-details-bin` with the detail packed as
 * `type.googleapis.com/w17.ErrorDetail`.
 *
 * The message is the author's own text: translation catalogs are not part of
 * the first version of the non-Go SDKs.
 *
 * How the runtime answers an error a handler throws:
 *
 * - a `StatusError` (`forUser`'s, or one a storage call threw and the handler
 *   let through) goes out as it is, details included — Go returns a storage
 *   status unchanged too;
 * - an SDK error (`DistxError`, `NoPrincipalError`, `ConfigError`) is a Go
 *   *returned* error: `UNKNOWN` with its message;
 * - anything else is a Go *panic*: reported, and answered `INTERNAL`
 *   `"<method>: internal error"` — the error text never reaches the caller.
 */

import { status as grpcStatus } from "@grpc/grpc-js";
import { create, fromBinary, toBinary, type Message } from "@bufbuild/protobuf";
import { BinaryReader, BinaryWriter, WireType } from "@bufbuild/protobuf/wire";
import { type AnyMessage, getDefaultPool } from "./descriptors.js";

/** gRPC status codes (grpc-js's enum). */
export const Status = grpcStatus;
export type Status = grpcStatus;

export const STATUS_DETAILS_KEY = "grpc-status-details-bin";
const TYPE_PREFIX = "type.googleapis.com/";

/** One `google.protobuf.Any`. */
export interface PackedAny {
  readonly typeUrl: string;
  readonly value: Uint8Array;
}

/** `google.rpc.Status`, decoded. */
export interface RpcStatus {
  readonly code: number;
  readonly message: string;
  readonly details: readonly PackedAny[];
}

export function encodeRpcStatus(st: RpcStatus): Uint8Array {
  const w = new BinaryWriter();
  if (st.code !== 0) w.tag(1, WireType.Varint).int32(st.code);
  if (st.message) w.tag(2, WireType.LengthDelimited).string(st.message);
  for (const d of st.details) {
    w.tag(3, WireType.LengthDelimited).fork();
    if (d.typeUrl) w.tag(1, WireType.LengthDelimited).string(d.typeUrl);
    if (d.value.length) w.tag(2, WireType.LengthDelimited).bytes(d.value);
    w.join();
  }
  return w.finish();
}

export function decodeRpcStatus(bytes: Uint8Array): RpcStatus {
  const r = new BinaryReader(bytes);
  let code = 0;
  let message = "";
  const details: PackedAny[] = [];
  while (r.pos < r.len) {
    const [no, wt] = r.tag();
    if (no === 1 && wt === WireType.Varint) code = r.int32();
    else if (no === 2 && wt === WireType.LengthDelimited) message = r.string();
    else if (no === 3 && wt === WireType.LengthDelimited) {
      const sub = new BinaryReader(r.bytes());
      let typeUrl = "";
      let value: Uint8Array = new Uint8Array();
      while (sub.pos < sub.len) {
        const [n2, w2] = sub.tag();
        if (n2 === 1 && w2 === WireType.LengthDelimited) typeUrl = sub.string();
        else if (n2 === 2 && w2 === WireType.LengthDelimited) value = sub.bytes();
        else sub.skip(w2);
      }
      details.push({ typeUrl, value });
    } else r.skip(wt);
  }
  return { code, message, details };
}

/** Pack a message as a `google.protobuf.Any`, as Go's `anypb.New`. */
export function pack(msg: Message): PackedAny {
  const schema = getDefaultPool().schema(msg.$typeName);
  return { typeUrl: TYPE_PREFIX + msg.$typeName, value: toBinary(schema, msg) };
}

/** Unpack an Any through the SDK's pool; undefined when its type is unknown here. */
export function unpack(any: PackedAny): AnyMessage | undefined {
  const name = any.typeUrl.slice(any.typeUrl.lastIndexOf("/") + 1);
  const schema = getDefaultPool().registry.getMessage(name);
  if (schema === undefined) return undefined;
  return fromBinary(schema, any.value) as AnyMessage;
}

/**
 * A gRPC status as an error. `message` is the status message; `statusDetails`
 * the raw `google.rpc.Status` from `grpc-status-details-bin`, relayed as
 * received (a detail unknown here may be known downstream).
 */
export class StatusError extends Error {
  override name = "StatusError";
  readonly code: Status;
  readonly statusDetails: Uint8Array | undefined;

  constructor(code: Status, message: string, statusDetails?: Uint8Array) {
    super(message);
    this.code = code;
    this.statusDetails = statusDetails;
  }

  /** The code's name ("FAILED_PRECONDITION"). */
  get codeName(): string {
    return Status[this.code] ?? String(this.code);
  }

  /** The packed details, decoded from `statusDetails`. */
  get details(): readonly PackedAny[] {
    return this.statusDetails ? decodeRpcStatus(this.statusDetails).details : [];
  }
}

/** The `w17.ErrorDetail` (`code`, `message`, `field`) of `forUser`. */
export function ErrorDetail(init: { code?: string; message?: string; field?: string }): AnyMessage {
  return create(getDefaultPool().schema("w17.ErrorDetail"), init) as AnyMessage;
}

/**
 * A status with a `w17.ErrorDetail`. Build it with {@link forUser}.
 *
 * `dev` is the operator's copy; the runtime prefixes it with the method being
 * served (`Svc.Method: dev`) when the error leaves the handler, as Go's
 * `method` argument does.
 */
export class UserError extends StatusError {
  override name = "UserError";
  readonly dev: string;
  readonly detail: AnyMessage;

  constructor(code: Status, dev: string, detail: AnyMessage) {
    super(code, dev, encodeRpcStatus({ code, message: dev, details: [pack(detail)] }));
    this.dev = dev;
    this.detail = detail;
  }
}

function toStatus(code: Status | number | string): Status {
  if (typeof code === "string") {
    const v = (Status as unknown as Record<string, number>)[code.toUpperCase()];
    if (typeof v !== "number") throw new RangeError(`for_user: unknown status code ${JSON.stringify(code)}`);
    return v;
  }
  if (Status[code] === undefined) throw new RangeError(`for_user: unknown status code ${code}`);
  return code;
}

/**
 * The error a handler throws to refuse with a sentence for a person.
 *
 *     throw forUser(Status.FAILED_PRECONDITION, "SUBSCRIPTION_INACTIVE",
 *                   "This subscription is not active.");
 *
 * `detailCode` is what a client dispatches on, `message` the sentence it
 * shows, `field` the request field it is about ("" for none). `dev` is the
 * operator's copy for logs and Sentry; it defaults to `message`.
 */
export function forUser(
  code: Status | number | string,
  detailCode: string,
  message: string,
  field = "",
  dev?: string,
): UserError {
  const st = toStatus(code);
  return new UserError(st, dev ?? message, ErrorDetail({ code: detailCode, message, field }));
}

/** The `w17.ErrorDetail` an error carries, or undefined. */
export function errorDetail(err: unknown): AnyMessage | undefined {
  if (err instanceof UserError) return err.detail;
  if (!(err instanceof StatusError)) return undefined;
  for (const d of err.details) {
    if (d.typeUrl.endsWith("/w17.ErrorDetail")) return unpack(d);
  }
  return undefined;
}
