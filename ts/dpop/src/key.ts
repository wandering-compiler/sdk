// The device key: an ECDSA P-256 pair held by WebCrypto.

import { jwkThumbprint, publicJwk, type PublicJwk } from "./jwk";

/** A device key ready to sign proofs. */
export interface DpopKey {
  /** The WebCrypto pair. The private key is non-extractable unless a store asked otherwise. */
  readonly keyPair: CryptoKeyPair;
  /** The public JWK every proof carries. */
  readonly jwk: PublicJwk;
  /** The RFC 7638 thumbprint of {@link jwk}: the device id. */
  readonly thumbprint: string;
}

const P256: EcKeyGenParams = { name: "ECDSA", namedCurve: "P-256" };

/**
 * Generates a P-256 device key pair. The private key is non-extractable: no
 * script, this one included, can read it back out of WebCrypto. `extractable`
 * is for a {@link KeyStore} that must write the key to a file (a server, a
 * CLI, a test) and is never the browser default.
 */
export async function generateKeyPair(extractable = false): Promise<CryptoKeyPair> {
  return crypto.subtle.generateKey(P256, extractable, ["sign", "verify"]);
}

/** Wraps a pair as a {@link DpopKey}: checks it is P-256 and computes the JWK and thumbprint. */
export async function dpopKey(keyPair: CryptoKeyPair): Promise<DpopKey> {
  const priv = keyPair.privateKey;
  const alg = priv.algorithm as EcKeyAlgorithm;
  if (
    priv.type !== "private" ||
    alg.name !== "ECDSA" ||
    alg.namedCurve !== "P-256" ||
    !priv.usages.includes("sign")
  ) {
    throw new Error("dpop: device key must be an ECDSA P-256 private key usable for sign");
  }
  const jwk = await publicJwk(keyPair.publicKey);
  return { keyPair, jwk, thumbprint: await jwkThumbprint({ ...jwk }) };
}

/**
 * Where the device key lives between page loads or process runs.
 *
 * `load` returns the stored pair or undefined; `save` replaces it; `clear`
 * forgets it (after a revocation, so the next sign-in makes a new key).
 */
export interface KeyStore {
  load(): Promise<CryptoKeyPair | undefined>;
  save(keyPair: CryptoKeyPair): Promise<void>;
  clear(): Promise<void>;
  /**
   * True for a store that writes the private key out (and so needs it
   * extractable). Absent or false: keys are generated non-extractable.
   */
  readonly needsExtractableKey?: boolean;
  /**
   * Stores `keyPair` unless the store already holds a pair, atomically, and
   * returns the pair the store holds afterwards — the given one or the one
   * that was there. A store shared by several pages at once (IndexedDB is
   * shared by every tab of an origin) implements it so that two tabs making
   * their first key at the same moment end up with ONE key, not one each.
   */
  saveIfAbsent?(keyPair: CryptoKeyPair): Promise<CryptoKeyPair>;
  /**
   * Deletes the stored pair if, and only if, its thumbprint is `thumbprint`,
   * atomically: true when it deleted, false when the store holds another pair
   * or none, undefined when it cannot tell (a record saved without its
   * thumbprint) — the caller then compares and deletes in two steps. A store
   * shared by several pages implements it so that a page discarding a revoked
   * key cannot delete the new key another page saved between the compare and
   * the delete.
   */
  clearIf?(thumbprint: string): Promise<boolean | undefined>;
}

/** The key a stored pair is, or undefined when it is not a usable P-256 pair. */
async function usableKey(pair: CryptoKeyPair | undefined): Promise<DpopKey | undefined> {
  if (!pair) return undefined;
  try {
    return await dpopKey(pair);
  } catch {
    return undefined;
  }
}

/**
 * Loads the stored key, or generates one and stores it. Concurrent callers on
 * the same wrapper share one call (as the fetch wrapper does).
 *
 * The key used is always the one the STORE holds once the call is done, not
 * the one this call generated: another page on the same store (a second tab)
 * may have made its own in the meantime, and a key that is not the stored one
 * is a device nobody else in this browser signs as. A store with
 * {@link KeyStore.saveIfAbsent} settles that atomically; any other store is
 * saved to and read back, so the last writer's key wins.
 */
export async function loadOrCreateKey(store: KeyStore): Promise<DpopKey> {
  const stored = await store.load();
  const found = await usableKey(stored);
  if (found) return found;
  // A stored record that is not a usable P-256 pair (another library's, a
  // damaged one) counts as absent and is replaced: refusing it would fail
  // every request for good.
  const pair = await generateKeyPair(store.needsExtractableKey === true);
  if (store.saveIfAbsent && !stored) {
    const held = await usableKey(await store.saveIfAbsent(pair));
    if (held) return held;
  }
  await store.save(pair);
  return (await usableKey(await store.load())) ?? dpopKey(pair);
}

/** A {@link KeyStore} in memory: for tests, and for a process that wants a fresh key every run. */
export class MemoryKeyStore implements KeyStore {
  private pair: CryptoKeyPair | undefined;

  load(): Promise<CryptoKeyPair | undefined> {
    return Promise.resolve(this.pair);
  }

  save(keyPair: CryptoKeyPair): Promise<void> {
    this.pair = keyPair;
    return Promise.resolve();
  }

  clear(): Promise<void> {
    this.pair = undefined;
    return Promise.resolve();
  }
}

/**
 * A {@link KeyStore} that uses `primary` until it fails once, then `fallback`
 * (default: a {@link MemoryKeyStore}) for the rest of its life. For a browser
 * where IndexedDB exists but refuses to work (some private modes, a blocked
 * or corrupt database): signing goes on with a key in memory instead of every
 * authenticated request failing.
 *
 * The fallback starts with the last pair the primary returned or stored, so a
 * primary that fails only later (a reload of the key after another tab signed
 * in) keeps the key the live session is bound to rather than switching to a
 * new one. `onFallback` is told once, with the first error.
 */
export class FallbackKeyStore implements KeyStore {
  private switched: Promise<void> | undefined;
  private last: CryptoKeyPair | undefined;

  constructor(
    private readonly primary: KeyStore,
    private readonly fallback: KeyStore = new MemoryKeyStore(),
    private readonly onFallback?: (err: unknown) => void,
  ) {}

  get needsExtractableKey(): boolean {
    return (this.switched ? this.fallback : this.primary).needsExtractableKey === true;
  }

  /** True once the primary store has failed and the fallback is in use. */
  get usingFallback(): boolean {
    return this.switched !== undefined;
  }

  private async switchOver(err: unknown): Promise<void> {
    if (this.last) {
      try {
        await this.fallback.save(this.last);
      } catch {
        // an empty fallback still signs, as a new device
      }
    }
    this.onFallback?.(err);
  }

  private async use<T>(op: (s: KeyStore, primary: boolean) => Promise<T>): Promise<T> {
    if (!this.switched) {
      try {
        return await op(this.primary, true);
      } catch (err) {
        // Concurrent failures switch once: the first starts the switch, every
        // caller waits for the fallback to be seeded before using it.
        this.switched ??= this.switchOver(err);
      }
    }
    await this.switched;
    return op(this.fallback, false);
  }

  load(): Promise<CryptoKeyPair | undefined> {
    return this.use(async (s, primary) => {
      const pair = await s.load();
      if (primary) this.last = pair;
      return pair;
    });
  }

  save(keyPair: CryptoKeyPair): Promise<void> {
    return this.use(async (s, primary) => {
      await s.save(keyPair);
      if (primary) this.last = keyPair;
    });
  }

  clear(): Promise<void> {
    return this.use(async (s, primary) => {
      await s.clear();
      if (primary) this.last = undefined;
    });
  }

  saveIfAbsent(keyPair: CryptoKeyPair): Promise<CryptoKeyPair> {
    return this.use(async (s, primary) => {
      let held: CryptoKeyPair;
      if (s.saveIfAbsent) {
        held = await s.saveIfAbsent(keyPair);
      } else {
        await s.save(keyPair);
        held = (await s.load()) ?? keyPair;
      }
      if (primary) this.last = held;
      return held;
    });
  }

  clearIf(thumbprint: string): Promise<boolean | undefined> {
    return this.use(async (s, primary) => {
      if (!s.clearIf) return undefined;
      const cleared = await s.clearIf(thumbprint);
      if (primary && cleared === true) this.last = undefined;
      return cleared;
    });
  }
}

/**
 * The key store a browser page wants by default: IndexedDB, falling back to
 * memory if it fails ({@link FallbackKeyStore}); memory alone where there is
 * no IndexedDB (Node, React Native), which makes every process a new device —
 * pass a persistent store there instead.
 */
export function defaultKeyStore(onFallback?: (err: unknown) => void): KeyStore {
  const idb = (globalThis as { indexedDB?: IDBFactory }).indexedDB;
  if (!idb) return new MemoryKeyStore();
  return new FallbackKeyStore(
    new IndexedDBKeyStore({ indexedDB: idb }),
    new MemoryKeyStore(),
    onFallback,
  );
}

/** Options of {@link IndexedDBKeyStore}. */
export interface IndexedDBKeyStoreOptions {
  /** Database name. Default `w17-dpop`. */
  dbName?: string;
  /** Object store name. Default `keys`. */
  storeName?: string;
  /** Record key, for more than one device key per origin. Default `device`. */
  key?: string;
  /** The IndexedDB factory. Default `globalThis.indexedDB`. */
  indexedDB?: IDBFactory;
}

/**
 * A {@link KeyStore} in the browser's IndexedDB. It stores the CryptoKeyPair
 * object itself: IndexedDB structured-clones a CryptoKey, non-extractable
 * ones included, so the private key is persisted without ever being
 * exported, and stays unreadable to script after a reload.
 */
export class IndexedDBKeyStore implements KeyStore {
  private readonly dbName: string;
  private readonly storeName: string;
  private readonly key: string;
  private readonly factory: IDBFactory | undefined;

  constructor(opts: IndexedDBKeyStoreOptions = {}) {
    this.dbName = opts.dbName ?? "w17-dpop";
    this.storeName = opts.storeName ?? "keys";
    this.key = opts.key ?? "device";
    this.factory = opts.indexedDB ?? (globalThis as { indexedDB?: IDBFactory }).indexedDB;
  }

  private open(): Promise<IDBDatabase> {
    const factory = this.factory;
    if (!factory) return Promise.reject(new Error("dpop: IndexedDB is not available"));
    return new Promise((resolve, reject) => {
      const req = factory.open(this.dbName, 1);
      req.onupgradeneeded = () => {
        if (!req.result.objectStoreNames.contains(this.storeName))
          req.result.createObjectStore(this.storeName);
      };
      req.onsuccess = () => resolve(req.result);
      req.onerror = () => reject(req.error ?? new Error("dpop: IndexedDB open failed"));
      req.onblocked = () => reject(new Error("dpop: IndexedDB open blocked"));
    });
  }

  private async run<T>(
    mode: IDBTransactionMode,
    op: (s: IDBObjectStore) => IDBRequest,
  ): Promise<T> {
    const db = await this.open();
    try {
      return await new Promise<T>((resolve, reject) => {
        const tx = db.transaction(this.storeName, mode);
        const req = op(tx.objectStore(this.storeName));
        let result: T;
        req.onsuccess = () => {
          result = req.result as T;
        };
        // Resolve on commit, not on the request: a write is durable only then.
        tx.oncomplete = () => resolve(result);
        tx.onerror = () =>
          reject(tx.error ?? req.error ?? new Error("dpop: IndexedDB request failed"));
        tx.onabort = () => reject(tx.error ?? new Error("dpop: IndexedDB transaction aborted"));
      });
    } finally {
      db.close();
    }
  }

  async load(): Promise<CryptoKeyPair | undefined> {
    const v = await this.run<unknown>("readonly", (s) => s.get(this.key));
    return isPair(v) ? { privateKey: v.privateKey, publicKey: v.publicKey } : undefined;
  }

  async save(keyPair: CryptoKeyPair): Promise<void> {
    const record = await keyRecord(keyPair);
    await this.run("readwrite", (s) => s.put(record, this.key));
  }

  /**
   * Compares the stored record's thumbprint (written with it at save time)
   * and deletes it in ONE readwrite transaction, so another tab cannot save a
   * new key between the compare and the delete.
   */
  async clearIf(thumbprint: string): Promise<boolean | undefined> {
    const db = await this.open();
    try {
      return await new Promise<boolean | undefined>((resolve, reject) => {
        const tx = db.transaction(this.storeName, "readwrite");
        const s = tx.objectStore(this.storeName);
        let result: boolean | undefined = false;
        const get = s.get(this.key);
        get.onsuccess = () => {
          const v: unknown = get.result;
          if (!isPair(v)) return;
          const stored = (v as { thumbprint?: unknown }).thumbprint;
          if (typeof stored !== "string") {
            result = undefined; // saved without one: the caller compares itself
            return;
          }
          if (stored === thumbprint) {
            s.delete(this.key);
            result = true;
          }
        };
        tx.oncomplete = () => resolve(result);
        tx.onerror = () => reject(tx.error ?? new Error("dpop: IndexedDB request failed"));
        tx.onabort = () => reject(tx.error ?? new Error("dpop: IndexedDB transaction aborted"));
      });
    } finally {
      db.close();
    }
  }

  async clear(): Promise<void> {
    await this.run("readwrite", (s) => s.delete(this.key));
  }

  /**
   * Reads and, when empty, writes in ONE readwrite transaction. IndexedDB
   * runs readwrite transactions over the same store one at a time across
   * every tab of the origin, so of two tabs creating a key at once, the
   * second finds the first's and returns it.
   */
  async saveIfAbsent(keyPair: CryptoKeyPair): Promise<CryptoKeyPair> {
    const record = await keyRecord(keyPair);
    const db = await this.open();
    try {
      return await new Promise<CryptoKeyPair>((resolve, reject) => {
        const tx = db.transaction(this.storeName, "readwrite");
        const s = tx.objectStore(this.storeName);
        let result: CryptoKeyPair = keyPair;
        const get = s.get(this.key);
        get.onsuccess = () => {
          const v: unknown = get.result;
          if (isPair(v)) {
            result = { privateKey: v.privateKey, publicKey: v.publicKey };
            return;
          }
          s.put(record, this.key);
        };
        tx.oncomplete = () => resolve(result);
        tx.onerror = () => reject(tx.error ?? new Error("dpop: IndexedDB request failed"));
        tx.onabort = () => reject(tx.error ?? new Error("dpop: IndexedDB transaction aborted"));
      });
    } finally {
      db.close();
    }
  }
}

/**
 * The record IndexedDBKeyStore keeps: the pair as a plain object (a
 * caller-built pair may carry other members) and its thumbprint, so that
 * clearIf can compare without computing one inside a transaction.
 */
async function keyRecord(keyPair: CryptoKeyPair) {
  return {
    privateKey: keyPair.privateKey,
    publicKey: keyPair.publicKey,
    thumbprint: (await dpopKey(keyPair)).thumbprint,
  };
}

function isPair(v: unknown): v is CryptoKeyPair {
  return !!v && typeof v === "object" && "privateKey" in v && "publicKey" in v;
}
