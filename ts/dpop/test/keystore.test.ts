import { mkdtemp, readFile, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { IDBFactory } from "fake-indexeddb";
import { describe, expect, it } from "vitest";

import {
  defaultKeyStore,
  dpopKey,
  FallbackKeyStore,
  generateKeyPair,
  IndexedDBKeyStore,
  loadOrCreateKey,
  MemoryKeyStore,
  type KeyStore,
} from "../src/key";
import { FileKeyStore } from "../src/node";
import { createProof } from "../src/proof";
import { verifyProof } from "./helpers";

/** A key from the store still signs proofs a server accepts. */
async function signsWith(pair: CryptoKeyPair): Promise<boolean> {
  return verifyProof(
    await createProof(await dpopKey(pair), { htm: "GET", htu: "https://a.example/" }),
  );
}

describe("MemoryKeyStore and loadOrCreateKey", () => {
  it("creates a key once, then loads the same one", async () => {
    const store = new MemoryKeyStore();
    expect(await store.load()).toBeUndefined();
    const a = await loadOrCreateKey(store);
    const b = await loadOrCreateKey(store);
    expect(b.thumbprint).toBe(a.thumbprint);
    expect(a.keyPair.privateKey.extractable).toBe(false);
    await store.clear();
    expect(await store.load()).toBeUndefined();
    expect((await loadOrCreateKey(store)).thumbprint).not.toBe(a.thumbprint);
  });

  it("generates an extractable key only for a store that asks", async () => {
    const inner = new MemoryKeyStore();
    const store: KeyStore = {
      needsExtractableKey: true,
      load: () => inner.load(),
      save: (p) => inner.save(p),
      clear: () => inner.clear(),
    };
    expect((await loadOrCreateKey(store)).keyPair.privateKey.extractable).toBe(true);
  });
});

describe("IndexedDBKeyStore", () => {
  it("persists a non-extractable key across store instances, still able to sign", async () => {
    const indexedDB = new IDBFactory();
    const first = await loadOrCreateKey(new IndexedDBKeyStore({ indexedDB }));
    expect(first.keyPair.privateKey.extractable).toBe(false);

    // A second instance stands for the next page load.
    const reloaded = new IndexedDBKeyStore({ indexedDB });
    const pair = await reloaded.load();
    expect(pair).toBeDefined();
    expect(pair!.privateKey.extractable).toBe(false);
    expect((await dpopKey(pair!)).thumbprint).toBe(first.thumbprint);
    expect(await signsWith(pair!)).toBe(true);

    await reloaded.clear();
    expect(await new IndexedDBKeyStore({ indexedDB }).load()).toBeUndefined();
  });

  it("keeps keys under different record keys apart", async () => {
    const indexedDB = new IDBFactory();
    const a = await loadOrCreateKey(new IndexedDBKeyStore({ indexedDB, key: "a" }));
    const b = await loadOrCreateKey(new IndexedDBKeyStore({ indexedDB, key: "b" }));
    expect(a.thumbprint).not.toBe(b.thumbprint);
    expect((await loadOrCreateKey(new IndexedDBKeyStore({ indexedDB, key: "a" }))).thumbprint).toBe(
      a.thumbprint,
    );
  });

  it("fails clearly where there is no IndexedDB", async () => {
    await expect(new IndexedDBKeyStore().load()).rejects.toThrow(/IndexedDB is not available/);
  });
});

describe("one key per store, whoever writes last", () => {
  it("uses the key the store holds after saving, not the one it generated", async () => {
    // Another page on the same store writes its key right after ours.
    const inner = new MemoryKeyStore();
    const other = await generateKeyPair();
    const store: KeyStore = {
      load: () => inner.load(),
      save: async (p) => {
        await inner.save(p);
        await inner.save(other);
      },
      clear: () => inner.clear(),
    };
    const key = await loadOrCreateKey(store);
    expect(key.thumbprint).toBe((await dpopKey(other)).thumbprint);
  });

  it("makes ONE key for two pages creating one at the same moment on IndexedDB", async () => {
    const indexedDB = new IDBFactory();
    const [a, b] = await Promise.all([
      loadOrCreateKey(new IndexedDBKeyStore({ indexedDB })),
      loadOrCreateKey(new IndexedDBKeyStore({ indexedDB })),
    ]);
    expect(a.thumbprint).toBe(b.thumbprint);
    const stored = await new IndexedDBKeyStore({ indexedDB }).load();
    expect((await dpopKey(stored!)).thumbprint).toBe(a.thumbprint);
  });

  it("saveIfAbsent keeps a pair that is already there", async () => {
    const indexedDB = new IDBFactory();
    const store = new IndexedDBKeyStore({ indexedDB });
    const first = await generateKeyPair();
    const second = await generateKeyPair();
    expect(await dpopKey(await store.saveIfAbsent(first))).toEqual(await dpopKey(first));
    const kept = await store.saveIfAbsent(second);
    expect((await dpopKey(kept)).thumbprint).toBe((await dpopKey(first)).thumbprint);
  });
});

describe("FallbackKeyStore and defaultKeyStore", () => {
  const broken: KeyStore = {
    load: () => Promise.reject(new Error("idb broken")),
    save: () => Promise.reject(new Error("idb broken")),
    clear: () => Promise.reject(new Error("idb broken")),
  };

  it("goes on in memory once the primary store fails, and says so once", async () => {
    const seen: unknown[] = [];
    const store = new FallbackKeyStore(broken, new MemoryKeyStore(), (e) => seen.push(e));
    const key = await loadOrCreateKey(store);
    expect(store.usingFallback).toBe(true);
    expect((await loadOrCreateKey(store)).thumbprint).toBe(key.thumbprint);
    await store.clear();
    expect(await store.load()).toBeUndefined();
    expect(seen).toHaveLength(1);
  });

  it("uses the primary store while it works", async () => {
    const primary = new MemoryKeyStore();
    const store = new FallbackKeyStore(primary);
    const key = await loadOrCreateKey(store);
    expect(store.usingFallback).toBe(false);
    expect((await dpopKey((await primary.load())!)).thumbprint).toBe(key.thumbprint);
  });

  it("defaultKeyStore: IndexedDB that fails to open still yields a working key", async () => {
    const g = globalThis as { indexedDB?: unknown };
    const saved = g.indexedDB;
    g.indexedDB = {
      open: () => {
        throw new Error("blocked");
      },
    };
    try {
      const store = defaultKeyStore();
      expect(store).toBeInstanceOf(FallbackKeyStore);
      const key = await loadOrCreateKey(store);
      expect(await signsWith(key.keyPair)).toBe(true);
    } finally {
      g.indexedDB = saved;
    }
  });

  it("defaultKeyStore: memory where there is no IndexedDB", () => {
    const g = globalThis as { indexedDB?: unknown };
    const saved = g.indexedDB;
    delete g.indexedDB;
    try {
      expect(defaultKeyStore()).toBeInstanceOf(MemoryKeyStore);
    } finally {
      if (saved !== undefined) g.indexedDB = saved;
    }
  });
});

describe("FileKeyStore", () => {
  const dir = () => mkdtemp(join(tmpdir(), "w17-dpop-"));

  it("must be told it writes the private key out", () => {
    expect(() => new FileKeyStore("/tmp/x", {} as never)).toThrow(/exportPrivateKey/);
    expect(() => new FileKeyStore("/tmp/x", { exportPrivateKey: "yes" } as never)).toThrow(
      /exportPrivateKey/,
    );
    expect(() => new FileKeyStore("", { exportPrivateKey: true })).toThrow(/path/);
  });

  it("writes a PKCS #8 PEM with mode 0600 and loads the same key back", async () => {
    const path = join(await dir(), "device.pem");
    const store = new FileKeyStore(path, { exportPrivateKey: true });
    expect(await store.load()).toBeUndefined();
    const created = await loadOrCreateKey(store);

    const pem = await readFile(path, "utf8");
    expect(pem).toMatch(
      /^-----BEGIN PRIVATE KEY-----\n([A-Za-z0-9+/=]{1,64}\n)+-----END PRIVATE KEY-----\n$/,
    );
    expect((await stat(path)).mode & 0o777).toBe(0o600);

    const pair = await new FileKeyStore(path, { exportPrivateKey: true }).load();
    expect((await dpopKey(pair!)).thumbprint).toBe(created.thumbprint);
    expect(await signsWith(pair!)).toBe(true);

    await store.clear();
    await store.clear(); // twice: a missing file is fine
    expect(await store.load()).toBeUndefined();
  });

  it("replaces an existing file rather than writing into it", async () => {
    const path = join(await dir(), "device.pem");
    await writeFile(path, "old", { mode: 0o644 });
    const store = new FileKeyStore(path, { exportPrivateKey: true });
    await store.save(await generateKeyPair(true));
    expect((await stat(path)).mode & 0o777).toBe(0o600);
    expect(await store.load()).toBeDefined();
  });

  it("refuses a file that holds no private key", async () => {
    const path = join(await dir(), "device.pem");
    await writeFile(path, "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n");
    await expect(new FileKeyStore(path, { exportPrivateKey: true }).load()).rejects.toThrow(
      /PRIVATE KEY/,
    );
  });
});
