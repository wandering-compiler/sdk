import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { type MessageType, type Pool, getDefaultPool } from "../src/descriptors.ts";

export const HERE = dirname(fileURLToPath(import.meta.url));
export const CONFORMANCE = join(HERE, "conformance");
export const DESCRIPTORS = join(CONFORMANCE, "testdata", "descriptors.binpb");
export const MANIFEST = join(CONFORMANCE, "service", "w17-business.json");
export const PROJECT = join(CONFORMANCE, "service");
export const TESTDATA = join(HERE, "testdata");

/** The process pool with the conformance descriptor set (adding it twice is a no-op). */
export function pool(): Pool {
  const p = getDefaultPool();
  p.addSetFile(DESCRIPTORS);
  return p;
}

export function msg(name: string): MessageType {
  return pool().message("conformance.v1." + name);
}

/** Run fn with a fresh empty working directory; restores the old one. */
export async function inTmpDir<T>(fn: (dir: string) => Promise<T> | T): Promise<T> {
  const old = process.cwd();
  const dir = mkdtempSync(join(tmpdir(), "w17-business-"));
  process.chdir(dir);
  try {
    return await fn(dir);
  } finally {
    process.chdir(old);
  }
}

export function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}

/** Poll cond until it holds or timeoutMs passes; returns its last value. */
export async function settled(cond: () => boolean, timeoutMs = 2000): Promise<boolean> {
  const end = Date.now() + timeoutMs;
  while (!cond() && Date.now() < end) await sleep(10);
  return cond();
}
