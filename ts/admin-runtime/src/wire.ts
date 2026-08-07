/**
 * Which HTTP wire the console speaks, and the codecs it needs to speak it.
 *
 * The admin surface has negotiated JSON and binary protobuf in both directions
 * since it moved onto the REST encoder (`docs/specs/admin/architecture.md`), so
 * this is only about what the SPA ASKS for. Protobuf is the default: the
 * console is a first-party client of a schema it was generated from, and there
 * is no third party to keep the wire readable for.
 *
 * The runtime itself stays generic. It ships no per-message code — the codecs
 * are generated per project into the SPA scaffold and handed in through
 * `bootstrap({ codecs })`, the same seam the spec already uses.
 */

/** One message's binary codec, as the generated `codecs.js` exposes it. */
export interface WireCodec {
  encode(value: unknown): Uint8Array;
  decode(bytes: Uint8Array): unknown;
}

/** Message ref → codec. Keys match the refs the spec names per endpoint. */
export type WireCodecs = Record<string, WireCodec>;

/** The media type the protobuf wire negotiates on, both directions. */
export const MIME_PROTOBUF = "application/protobuf";

/**
 * The query parameter that forces JSON for this browser, and the storage key
 * it sticks to.
 *
 * This is the debugging affordance the wire switch would otherwise be asked
 * for: an operator reporting a bug appends `?w17wire=json`, reloads, and every
 * subsequent request is readable in devtools — no redeploy, no lock edit, and
 * no need to reach whoever owns the project's config. `?w17wire=pb` puts it
 * back; anything else is ignored rather than treated as "off", so a typo
 * cannot silently change the wire.
 */
export const WIRE_PARAM = "w17wire";
const WIRE_STORAGE_KEY = "w17.admin.wire";

/**
 * Resolves the wire for this session: the URL wins and is remembered, then
 * what was remembered, then what the spec was generated for.
 *
 * Reading the URL has a side effect on purpose — a wire that reset on the next
 * navigation would be useless for the case it exists for, which is clicking
 * around an admin while watching the network tab.
 */
export function resolveWire(specWirePB: boolean): boolean {
  const override = readOverride();
  if (override !== null) return override;
  return specWirePB;
}

function readOverride(): boolean | null {
  if (typeof window === "undefined") return null;
  let stored: string | null = null;
  try {
    stored = window.localStorage?.getItem(WIRE_STORAGE_KEY) ?? null;
  } catch {
    // A browser with storage disabled still gets the per-load override below;
    // it just does not stick. Losing the preference is not worth failing boot.
    stored = null;
  }
  const fromUrl = new URLSearchParams(window.location.search).get(WIRE_PARAM);
  const picked = fromUrl === "json" || fromUrl === "pb" ? fromUrl : stored;
  if (fromUrl === "json" || fromUrl === "pb") {
    try {
      window.localStorage?.setItem(WIRE_STORAGE_KEY, fromUrl);
    } catch {
      // See above — the override still applies to this load.
    }
  }
  if (picked === "json") return false;
  if (picked === "pb") return true;
  return null;
}

/**
 * The wire the runtime actually uses, resolved once at boot and read by the
 * fetch layer. A module-level value rather than context because `api.ts` is
 * plain functions called from everywhere, and threading a provider through
 * every call site would be a lot of churn to express one boot-time constant.
 */
let activeCodecs: WireCodecs = {};
let activePB = false;

/** Called once by bootstrap. */
export function configureWire(codecs: WireCodecs | undefined, specWirePB: boolean): void {
  activeCodecs = codecs ?? {};
  // No codecs means nothing CAN be decoded, whatever the spec asked for — a
  // project generated before the codec emit, or one whose endpoints are all
  // well-known types. Falling back rather than failing keeps the console
  // working; it is the same degradation as a single endpoint with no codec.
  activePB = specWirePB && Object.keys(activeCodecs).length > 0;
}

/** Reports whether a given message ref can ride the binary wire. */
export function codecFor(ref: string | undefined): WireCodec | undefined {
  if (!activePB || !ref) return undefined;
  return activeCodecs[ref];
}

/** Whether the console is on the binary wire at all. Exposed for diagnostics. */
export function wireIsPB(): boolean {
  return activePB;
}
