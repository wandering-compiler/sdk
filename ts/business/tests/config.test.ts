import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { inspect } from "node:util";
import { Encrypter, generateX25519Identity, identityToRecipient } from "age-encryption";
import { describe, expect, it } from "vitest";
import { Config, ConfigError } from "../src/config.ts";
import { type ConfigKey, ManifestError, fromObject, load } from "../src/manifest.ts";
import * as secrets from "../src/secrets.ts";
import { MANIFEST, TESTDATA, inTmpDir } from "./helpers.ts";

const key = (k: Partial<ConfigKey> & { name: string; env: string }): ConfigKey => ({
  type: "string",
  default: "",
  required: false,
  secret: false,
  description: "",
  ...k,
});

const KEYS: ConfigKey[] = [
  key({ name: "greeting", env: "N_GREETING", default: "hello" }),
  key({ name: "limit", env: "N_LIMIT", type: "int", default: "10" }),
  key({ name: "verbose", env: "N_VERBOSE", type: "bool", default: "false" }),
  key({ name: "token", env: "N_TOKEN", secret: true }),
];

describe("config", () => {
  it("defaults and typed getters", () =>
    inTmpDir(async () => {
      const cfg = await Config.load(KEYS, {});
      expect(cfg.str("greeting")).toBe("hello");
      expect(cfg.int("limit")).toBe(10);
      expect(cfg.bool("verbose")).toBe(false);
      expect(cfg.secret("token").isZero()).toBe(true);
    }));

  it("values parse like Go", () =>
    inTmpDir(async () => {
      const cfg = await Config.load(KEYS, { N_GREETING: "hi", N_LIMIT: "-3", N_VERBOSE: "1", N_TOKEN: "s3cret" });
      expect([cfg.str("greeting"), cfg.int("limit"), cfg.bool("verbose")]).toEqual(["hi", -3, true]);
      expect(cfg.secret("token").reveal()).toBe("s3cret");
      expect((await Config.load(KEYS, { N_LIMIT: "+4" })).int("limit")).toBe(4);
    }));

  it.each([
    [{ N_LIMIT: "ten" }, "N_LIMIT"],
    [{ N_LIMIT: "1.5" }, "N_LIMIT"],
    [{ N_VERBOSE: "yes" }, "N_VERBOSE"],
    [{ N_VERBOSE: "True" }, "N_VERBOSE"],
  ])("an unparseable value stops the boot naming the variable (%j)", (env, name) =>
    inTmpDir(async () => {
      await expect(Config.load(KEYS, env)).rejects.toThrow(ConfigError);
      await expect(Config.load(KEYS, env)).rejects.toThrow(name);
    }));

  it("a required key missing names the key and the variable", () =>
    inTmpDir(async () => {
      const keys = [
        ...KEYS,
        key({ name: "api_url", env: "N_API_URL", required: true }),
        key({ name: "api_key", env: "N_API_KEY", secret: true, required: true }),
      ];
      await expect(Config.load(keys, { N_API_KEY: "k" })).rejects.toThrow(/'api_url'.*N_API_URL is required/);
      await expect(Config.load(keys, { N_API_URL: "u" })).rejects.toThrow(/'api_key'.*N_API_KEY is required/);
    }));

  it("a secret never prints", () =>
    inTmpDir(async () => {
      const cfg = await Config.load(KEYS, { N_TOKEN: "s3cret" });
      const s = cfg.secret("token");
      expect([String(s), `${s}`, JSON.stringify(s), inspect(s)]).toEqual(["***", "***", '"***"', "***"]);
      expect(String(cfg)).not.toContain("s3cret");
      expect(JSON.stringify(cfg)).not.toContain("s3cret");
      expect(inspect(cfg)).not.toContain("s3cret");
    }));

  it("an undeclared or mistyped key throws", () =>
    inTmpDir(async () => {
      const cfg = await Config.load(KEYS, {});
      expect(() => cfg.str("nope")).toThrow(RangeError);
      expect(() => cfg.str("limit")).toThrow(TypeError);
      expect(cfg.get("limit")).toBe(10n);
    }));

  it.each(["9223372036854775808", "-9223372036854775809", "1" + "0".repeat(30), "9".repeat(5000)])(
    "an int out of the int64 range is refused like Go's Atoi (%#)",
    (raw) =>
      inTmpDir(async () => {
        await expect(Config.load(KEYS, { N_LIMIT: raw })).rejects.toThrow(/N_LIMIT.*out of range/);
        expect((await Config.load(KEYS, { N_LIMIT: "9223372036854775807" })).bigint("limit")).toBe(2n ** 63n - 1n);
        expect((await Config.load(KEYS, { N_LIMIT: "-9223372036854775808" })).bigint("limit")).toBe(-(2n ** 63n));
      }),
  );

  it("int() refuses a value a JavaScript number cannot hold exactly; bigint() reads it", () =>
    inTmpDir(async () => {
      const cfg = await Config.load(KEYS, { N_LIMIT: "9007199254740993" });
      expect(() => cfg.int("limit")).toThrow(/beyond 2\^53/);
      expect(cfg.bigint("limit")).toBe(9007199254740993n);
    }));
});

// --- secrets ------------------------------------------------------------------

async function ageFile(dir: string, content: string): Promise<string> {
  const ident = await generateX25519Identity();
  const e = new Encrypter();
  e.addRecipient(await identityToRecipient(ident));
  writeFileSync(join(dir, ".secrets.age"), await e.encrypt(content));
  return ident;
}

describe("secrets", () => {
  it("parses dotenv like Go", () => {
    const kv = secrets.parseDotenv('# c\n\nA=1\n B = two \nC=x=y\r\nNOEQ\n=v\nD="q"\n');
    expect(kv).toEqual({ A: "1", B: " two ", C: "x=y", D: '"q"' });
  });

  it("chains env, then the age file, then the plain file", () =>
    inTmpDir(async (dir) => {
      const k = await ageFile(dir, "A=age\nB=age\n");
      writeFileSync(join(dir, ".secrets"), "A=plain\nB=plain\nC=plain\n");
      const r = await secrets.newResolver({ W17_SECRETS_AGE_KEY: k, A: "env" });
      expect([r.get("A"), r.get("B"), r.get("C"), r.get("D")]).toEqual(["env", "age", "plain", ""]);
    }));

  it("reads an age key file with comment lines", () =>
    inTmpDir(async (dir) => {
      const k = await ageFile(dir, "A=age\n");
      writeFileSync(join(dir, "key.txt"), `# created: now\n# public key: x\n${k}\n`);
      const r = await secrets.newResolver({ W17_SECRETS_AGE_KEY_FILE: join(dir, "key.txt") });
      expect(r.get("A")).toBe("age");
    }));

  it("auto without a key skips the age file", () =>
    inTmpDir(async (dir) => {
      await ageFile(dir, "A=age\n");
      expect((await secrets.newResolver({})).get("A")).toBe("");
    }));

  it("mode age requires a key", () =>
    inTmpDir(async () => {
      await expect(secrets.newResolver({ W17_SECRETS_MODE: "age" })).rejects.toThrow("mode=age but no age key");
    }));

  it("a wrong key fails loudly", () =>
    inTmpDir(async (dir) => {
      await ageFile(dir, "A=age\n");
      const other = await generateX25519Identity();
      await expect(secrets.newResolver({ W17_SECRETS_AGE_KEY: other })).rejects.toThrow("wrong key");
    }));

  it("mode plain ignores the age file", () =>
    inTmpDir(async (dir) => {
      const k = await ageFile(dir, "A=age\n");
      writeFileSync(join(dir, ".secrets"), "A=plain\n");
      const r = await secrets.newResolver({ W17_SECRETS_MODE: "plain", W17_SECRETS_AGE_KEY: k });
      expect(r.get("A")).toBe("plain");
    }));

  it("an unknown mode is refused", () =>
    inTmpDir(async () => {
      await expect(secrets.newResolver({ W17_SECRETS_MODE: "vault" })).rejects.toThrow("unknown W17_SECRETS_MODE");
    }));

  it("a secret config key resolves through the chain", () =>
    inTmpDir(async (dir) => {
      writeFileSync(join(dir, ".secrets"), "N_TOKEN=from-file\n");
      expect((await Config.load(KEYS, {})).secret("token").reveal()).toBe("from-file");
    }));
});

// --- manifest -----------------------------------------------------------------

const min = (kw: Record<string, unknown> = {}) => ({
  format: 1,
  domain: "d",
  language: "typescript",
  register: { path: "biz/d" },
  ...kw,
});

describe("manifest", () => {
  it("the conformance manifest loads", () => {
    const m = load(MANIFEST);
    expect(m.domain).toBe("notes");
    expect(m.register).toEqual({ path: "conformance_notes", package: "conformance_notes", module: "w17.ts", function: "registerBusiness" });
    expect(m.events!.emits[0]!.fields[2]!.value).toBe("conformance");
    expect(m.events!.emits[0]!.envelope).toBe("conformance.events.Event");
    expect(m.storage!.distributedTransactions).toBe("w17.common.distx.W17DistributedTransaction");
    expect(m.config.filter((k) => k.required).map((k) => k.name)).toEqual(["api_token"]);
    expect(m.shutdownTimeoutMs).toBe(10_000);
  });

  it("the spec's example reads as specified (its language set to typescript)", async () => {
    // The example manifest of the platform's business-manifest spec, verbatim
    // but for the language and the register this SDK reads.
    const { readFileSync } = await import("node:fs");
    const d = JSON.parse(readFileSync(join(TESTDATA, "shop.json"), "utf8")) as Record<string, unknown>;
    d.language = "typescript";
    d.register = { path: "business/shop", package: "shop", module: "w17.ts", function: "registerBusiness" };
    const m = fromObject(d, TESTDATA);
    expect([m.bundle, m.envPrefix, m.listenEnv, m.defaultListenAddr]).toEqual(["shop-business", "SHOP", "SHOP_LISTEN_ADDR", ":50051"]);
    expect([m.shutdownTimeoutMs, m.descriptorSet]).toEqual([10_000, "descriptors.binpb"]);
    expect([m.register.path, m.register.module, m.register.function]).toEqual(["business/shop", "w17.ts", "registerBusiness"]);
    const svc = m.services[0]!;
    expect(svc.name).toBe("shop.core.CheckoutService");
    expect(svc.methods.map((x) => x.name)).toEqual(["PlaceOrder", "Watch", "Cancel"]);
    expect(svc.methods[1]!.serverStreaming && !svc.methods[0]!.serverStreaming).toBe(true);
    const cfg = Object.fromEntries(m.config.map((k) => [k.name, k]));
    // Typed defaults become the text a variable would hold.
    expect([cfg.max_items!.default, cfg.dry_run!.default, cfg.api_key!.secret]).toEqual(["25", "true", true]);
    await inTmpDir(async () => {
      expect((await Config.load(m.config, {})).int("max_items")).toBe(25);
      expect((await Config.load(m.config, {})).bool("dry_run")).toBe(true);
    });
    expect(m.storage!.addressEnv).toBe("SHOP_STORAGE_BACKEND_ADDR");
    expect(m.storage!.services).toEqual(["shop.core.OrderMutation", "shop.core.OrderQuery"]);
    const ev = m.events!;
    expect([ev.natsDsnEnv, ev.redisDsnEnv, ev.redisChannels]).toEqual(["SHOP_EVENTBUS_NATS_DSN", "", []]);
    const e = ev.emits[0]!;
    expect([e.method, e.event, e.envelope, e.envelopeField, e.channel, e.topic]).toEqual([
      "/shop.core.CheckoutService/PlaceOrder", "shop.OrderPlaced", "w17lock.shop.events.Event",
      "shop_order_placed", "default", "order.placed",
    ]);
    expect(e.fields.map((f) => [f.field, f.source, f.path, f.value])).toEqual([
      ["order_id", "response", ["order_id"], ""],
      ["source", "literal", [], "checkout"],
      ["total", "request", ["cart", "total"], ""],
    ]);
    const inv = ev.invalidates[0]!;
    expect([inv.method, inv.topic, inv.group, inv.entities]).toEqual(["/shop.core.CheckoutService/Cancel", "w17.invalidate", "shop.core", ["Order"]]);
    expect(m.pluginHooks).toEqual([]);
  });

  it("format and language are checked", () => {
    expect(() => fromObject(min({ format: 3 }))).toThrow(/reads formats 1 and 2/);
    expect(fromObject(min({ format: 2 })).companion).toBeNull();
    const { format: _f, ...noFormat } = min();
    expect(() => fromObject(noFormat)).toThrow(/format/);
    expect(() => fromObject(min({ language: "python" }))).toThrow(/not typescript/);
    // An absent language is Python's default: refused here too.
    const { language: _l, ...noLanguage } = min();
    expect(() => fromObject(noLanguage)).toThrow(/not typescript/);
  });

  it("defaults, nulls and unknown fields", () => {
    const m = fromObject(min({ storage: null, events: null, future: 1, config: [{ name: "x" }] }));
    expect([m.listenEnv, m.reflectionEnv]).toEqual(["D_LISTEN_ADDR", "D_GRPC_REFLECTION"]);
    expect(m.register).toEqual({ path: "biz/d", package: "d", module: "w17.ts", function: "registerBusiness" });
    expect([m.storage, m.events]).toEqual([null, null]);
    expect(m.config[0]!.env).toBe("D_X");
    expect(m.extra).toEqual({ future: 1 });
  });

  it("an int default beyond 2^53 stays exact", () =>
    inTmpDir(async (dir) => {
      const p = join(dir, "m.json");
      const base = JSON.stringify(min()).slice(0, -1);
      writeFileSync(
        p,
        `${base},"config":[{"name":"big","type":"int","default":9007199254740993},` +
          `{"name":"neg","type":"int","default":-9223372036854775808},{"name":"obj","default":{"n":9007199254740993,"f":1.5}}]}`,
      );
      const m = load(p);
      expect(m.config.map((k) => k.default)).toEqual(["9007199254740993", "-9223372036854775808", '{"n":9007199254740993,"f":1.5}']);
      const cfg = await Config.load(m.config, {});
      expect(cfg.bigint("big")).toBe(9007199254740993n);
      expect(cfg.bigint("neg")).toBe(-9223372036854775808n);
    }));

  it("required fields", () =>
    inTmpDir((dir) => {
      const p = join(dir, "m.json");
      writeFileSync(p, JSON.stringify({ format: 1, domain: "d", language: "typescript" }));
      expect(() => load(p)).toThrow(/register is required/);
      writeFileSync(p, "{nope");
      expect(() => load(p)).toThrow(/not JSON/);
      expect(() => load(join(dir, "missing.json"))).toThrow(ManifestError);
      expect(() =>
        fromObject(
          min({ events: { envelope: "", emits: [{ method: "/a.S/M", event: "a.E", envelope_field: "e", channel: "c", topic: "t" }] } }),
        ),
      ).toThrow(/envelope is required/);
    }));
});
