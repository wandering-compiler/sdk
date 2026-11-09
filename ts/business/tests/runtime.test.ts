// Booting the conformance tier from its manifest: config failures, health,
// reflection, the drain, internal TLS, telemetry, the CLI.

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { Client, Metadata as GrpcMetadata, credentials } from "@grpc/grpc-js";
import { afterEach, describe, expect, it, vi } from "vitest";
import { main, parseDuration, probe, probeTarget } from "../src/cli.ts";
import { Conn, ServiceClient } from "../src/client.ts";
import { Context } from "../src/context.ts";
import { Status } from "../src/errors.ts";
import * as health from "../src/health.ts";
import { type Manifest, load } from "../src/manifest.ts";
import * as observability from "../src/observability.ts";
import { App, BootError } from "../src/runtime.ts";
import type { Env } from "../src/secrets.ts";
import * as tlsconfig from "../src/tlsconfig.ts";
import * as txscope from "../src/txscope.ts";
import { MANIFEST, PROJECT, TESTDATA, inTmpDir, msg, sleep } from "./helpers.ts";

vi.mock("@sentry/node", () => ({
  init: vi.fn(),
  flush: vi.fn(async () => true),
  captureException: vi.fn(),
  captureMessage: vi.fn(),
}));

function env(kw: Record<string, string | undefined> = {}): Env {
  const base: Record<string, string | undefined> = {
    NOTES_API_TOKEN: "t0ken",
    NOTES_LISTEN_ADDR: "127.0.0.1:0",
    NOTES_STORAGE_BACKEND_ADDR: "127.0.0.1:1",
    ...kw,
  };
  return Object.fromEntries(Object.entries(base).filter(([, v]) => v !== undefined));
}

const quiet = () => {
  vi.spyOn(console, "log").mockImplementation(() => undefined);
  vi.spyOn(console, "error").mockImplementation(() => undefined);
};

afterEach(() => vi.restoreAllMocks());

describe("boot", () => {
  it("a missing required key stops it", () =>
    inTmpDir(async () => {
      const app = new App(load(MANIFEST), { projectRoot: PROJECT, environ: env({ NOTES_API_TOKEN: undefined }) });
      await expect(app.boot()).rejects.toThrow(/'api_token'.*NOTES_API_TOKEN is required/);
    }));

  it("a bad value stops it", () =>
    inTmpDir(async () => {
      const app = new App(load(MANIFEST), { projectRoot: PROJECT, environ: env({ NOTES_LIMIT: "many" }) });
      await expect(app.boot()).rejects.toThrow(/NOTES_LIMIT/);
    }));

  it("a bad internal TLS value stops the start", () =>
    inTmpDir(async () => {
      quiet();
      const app = new App(load(MANIFEST), { projectRoot: PROJECT, environ: env({ W17_INTERNAL_TLS: "maybe" }) });
      await app.boot();
      await expect(app.start()).rejects.toThrow(/W17_INTERNAL_TLS/);
      app.clients?.close();
    }));

  it("a plugin hook without a handler stops it", () =>
    inTmpDir(async () => {
      quiet();
      const m: Manifest = {
        ...load(MANIFEST),
        pluginHooks: [{ plugin: "auth", registeredAs: "auth", slot: "after_signup", methods: ["/conformance.v1.NoteService/Nope"] }],
      };
      await expect(new App(m, { projectRoot: PROJECT, environ: env() }).boot()).rejects.toThrow(/after_signup.*Nope.*no handler/);
    }));

  it("registering a service the manifest does not list is refused", () =>
    inTmpDir(async () => {
      const m: Manifest = { ...load(MANIFEST), services: [] };
      await expect(new App(m, { projectRoot: PROJECT, environ: env() }).boot()).rejects.toThrow(/not a business service of this tier/);
    }));

  it("a missing register module names it", () =>
    inTmpDir(async (dir) => {
      const m: Manifest = { ...load(MANIFEST), register: { path: "nowhere/pkg_x", package: "pkg_x", module: "w17.ts", function: "registerBusiness" } };
      await expect(new App(m, { projectRoot: dir, environ: env() }).boot()).rejects.toThrow(/import nowhere\/pkg_x\/w17\.ts/);
    }));

  it("a broken client TLS config stops it", () =>
    inTmpDir(async () => {
      const app = new App(load(MANIFEST), { projectRoot: PROJECT, environ: env({ W17_INTERNAL_TLS: "on", W17_INTERNAL_TLS_CA: "/nonexistent" }) });
      await expect(app.boot()).rejects.toThrow(BootError);
      await expect(
        new App(load(MANIFEST), { projectRoot: PROJECT, environ: env({ W17_INTERNAL_TLS: "on", W17_INTERNAL_TLS_CA: "/nonexistent" }) }).boot(),
      ).rejects.toThrow(/W17_INTERNAL_TLS_CA/);
    }));
});

describe("serving", () => {
  it("serves health and reflection, and drains", () =>
    inTmpDir(async () => {
      quiet();
      const app = new App(load(MANIFEST), { projectRoot: PROJECT, environ: env({ NOTES_GRPC_REFLECTION: "1" }) });
      await app.boot();
      await app.start();
      const hook: (number | undefined)[] = [];
      app.registry!.registerShutdown((ctx) => void hook.push(ctx.timeRemaining()));
      expect(await health.check(`127.0.0.1:${app.port}`, credentials.createInsecure(), 2000)).toBe(health.ServingStatus.SERVING);
      await probe(`127.0.0.1:${app.port}`, {}, 2000);
      // Reflection lists the business service.
      const c = new Client(`127.0.0.1:${app.port}`, credentials.createInsecure());
      const listed = await new Promise<string>((res, rej) => {
        const call = c.makeBidiStreamRequest("/grpc.reflection.v1.ServerReflection/ServerReflectionInfo", (b: Buffer) => b, (b: Buffer) => b, new GrpcMetadata());
        call.on("data", (b: Buffer) => {
          res(b.toString("latin1"));
          call.end();
        });
        call.on("error", rej);
        call.write(Buffer.from([0x3a, 0x00])); // list_services = ""
      });
      c.close();
      expect(listed).toContain("conformance.v1.NoteService");
      await app.shutdown();
      expect(hook.length).toBe(1);
      expect(hook[0]!).toBeGreaterThan(0);
      expect(hook[0]!).toBeLessThanOrEqual(30_000); // the hook gets the drain deadline
    }));

  it("a reflection request that does not decode ends its call, not the process", () =>
    inTmpDir(async () => {
      quiet();
      const app = new App(load(MANIFEST), { projectRoot: PROJECT, environ: env({ NOTES_GRPC_REFLECTION: "1" }) });
      await app.boot();
      await app.start();
      const c = new Client(`127.0.0.1:${app.port}`, credentials.createInsecure());
      const ask = (b: Buffer) =>
        new Promise<{ code?: number; data?: string }>((res) => {
          const call = c.makeBidiStreamRequest("/grpc.reflection.v1.ServerReflection/ServerReflectionInfo", (x: Buffer) => x, (x: Buffer) => x, new GrpcMetadata());
          call.on("data", (d: Buffer) => {
            res({ data: d.toString("latin1") });
            call.end();
          });
          call.on("error", (e: { code: number }) => res({ code: e.code }));
          call.write(b);
        });
      // file_containing_symbol whose length runs past the message.
      expect((await ask(Buffer.from([0x1a, 0x32, 0x61, 0x62]))).code).toBe(Status.INVALID_ARGUMENT);
      // The server is still there and still answers.
      expect((await ask(Buffer.from([0x3a, 0x00]))).data).toContain("conformance.v1.NoteService");
      c.close();
      await app.shutdown();
    }));

  it("the drain waits for in-flight calls", () =>
    inTmpDir(async () => {
      quiet();
      const app = new App(load(MANIFEST), { projectRoot: PROJECT, environ: env() });
      await app.boot();
      await app.start();
      const conn = new Conn(`127.0.0.1:${app.port}`, { tls: false });
      const client = new ServiceClient(conn, "conformance.v1.NoteService", app.pool);
      const call = client.Sleep(new Context(), msg("SleepRequest")({ ms: 300 }));
      await sleep(100);
      await app.shutdown();
      expect((await call).text).toBe("slept"); // an in-flight call finishes during the drain
      conn.close();
    }));

  it("the drain waits for work the server's close started, before the bus closes", () =>
    inTmpDir(async () => {
      // A handler that commits as the server closes starts its events then.
      // The drain must come after that, and the bus must close only once
      // they are done.
      quiet();
      const app = new App(load(MANIFEST), { projectRoot: PROJECT, environ: env() });
      await app.boot();
      await app.start();
      const done: number[] = [];
      const atClose: number[][] = [];
      const srv = app.server!;
      const real = srv.tryShutdown.bind(srv);
      srv.tryShutdown = (cb) => {
        void txscope.shielded(sleep(100).then(() => void done.push(1)));
        real(cb);
      };
      app.dispatcher = { dispatch: async () => {}, close: async () => void atClose.push([...done]) };
      await app.shutdown();
      expect(atClose).toEqual([[1]]);
    }));
});

describe("the health command", () => {
  it("the probe target matches Go", () => {
    expect(probeTarget(":50061")).toEqual(["127.0.0.1", 50061]);
    expect(probeTarget("0.0.0.0:1")).toEqual(["127.0.0.1", 1]);
    expect(probeTarget("[::]:2")).toEqual(["127.0.0.1", 2]);
    expect(() => probeTarget(":0")).toThrow();
  });

  it("parses Go durations", () => {
    expect([parseDuration("3s"), parseDuration("500ms"), parseDuration("1m")]).toEqual([3000, 500, 60_000]);
    expect(() => parseDuration("3")).toThrow();
  });

  it("fails when nothing listens", async () => {
    const out: string[] = [];
    vi.spyOn(console, "log").mockImplementation((...a: unknown[]) => void out.push(a.join(" ")));
    vi.spyOn(console, "error").mockImplementation(() => undefined);
    vi.stubEnv("NOTES_LISTEN_ADDR", "127.0.0.1:1");
    try {
      expect(await main(["health", "--manifest", MANIFEST, "--timeout", "1s"])).toBe(1);
    } finally {
      vi.unstubAllEnvs();
    }
    expect(out.join("\n")).toContain("FAIL grpc 127.0.0.1:1");
  });

  it("serve reports a boot error", () =>
    inTmpDir(async () => {
      const err: string[] = [];
      vi.spyOn(console, "error").mockImplementation((...a: unknown[]) => void err.push(a.join(" ")));
      vi.stubEnv("NOTES_API_TOKEN", "");
      vi.stubEnv("NOTES_LISTEN_ADDR", "127.0.0.1:0");
      try {
        expect(await main(["serve", "--manifest", MANIFEST, "--project-root", PROJECT])).toBe(1);
      } finally {
        vi.unstubAllEnvs();
      }
      expect(err.join("\n")).toContain("NOTES_API_TOKEN is required");
    }));

  it("refuses an unknown command or flag", async () => {
    vi.spyOn(console, "error").mockImplementation(() => undefined);
    expect(await main(["nope"])).toBe(2);
    expect(await main(["health", "--project-root", "x"])).toBe(2);
    expect(await main(["health", "--timeout", "0s"])).toBe(2);
  });
});

// --- internal TLS: only the configured trust, and no silent half-config -------

const TLS = join(TESTDATA, "tls");
const CA = join(TLS, "ca.pem");
const LEAF = join(TLS, "leaf.pem");
const KEY = join(TLS, "leaf.key");
const ON = { [tlsconfig.ENV_INTERNAL_TLS]: "on" };

describe("internal TLS", () => {
  it("the values", () => {
    for (const v of ["", "off", "0", "no", "false"]) {
      tlsconfig.check(v);
      expect(tlsconfig.serverTLS({ [tlsconfig.ENV_INTERNAL_TLS]: v }).tls).toBe(false);
      expect(tlsconfig.clientTLS({ [tlsconfig.ENV_INTERNAL_TLS]: v }).tls).toBe(false);
    }
    expect(tlsconfig.serverTLS({ [tlsconfig.ENV_INTERNAL_TLS]: "terminated" }).tls).toBe(false);
    expect(tlsconfig.clientTLS({ [tlsconfig.ENV_INTERNAL_TLS]: "terminated" })).toEqual({ tls: true });
    expect(() => tlsconfig.serverTLS(ON)).toThrow(/not both set/);
    // An unrecognised value dials TLS rather than plaintext.
    expect(tlsconfig.clientTLS({ [tlsconfig.ENV_INTERNAL_TLS]: "maybe" }).tls).toBe(true);
  });

  it("a client trusts only the configured CA", () => {
    const c = tlsconfig.clientTLS({ ...ON, [tlsconfig.ENV_INTERNAL_TLS_CA]: CA });
    expect(c.rootCerts).toEqual(readFileSync(CA)); // that CA alone: grpc-js then trusts nothing else
  });

  it("without a CA a client uses the system roots, like Go", () => {
    expect(tlsconfig.clientTLS(ON).rootCerts).toBeUndefined();
    // "terminated" verifies the platform's certificate against them, whatever CA the stack has.
    expect(tlsconfig.clientTLS({ [tlsconfig.ENV_INTERNAL_TLS]: "terminated", [tlsconfig.ENV_INTERNAL_TLS_CA]: CA }).rootCerts).toBeUndefined();
  });

  it("a server verifies clients against the client CA only", () => {
    const s = tlsconfig.serverTLS({ ...ON, [tlsconfig.ENV_INTERNAL_TLS_CERT]: LEAF, [tlsconfig.ENV_INTERNAL_TLS_KEY]: KEY, [tlsconfig.ENV_INTERNAL_TLS_CLIENT_CA]: CA });
    expect(s.clientCA).toEqual(readFileSync(CA));
    const plain = tlsconfig.serverTLS({ ...ON, [tlsconfig.ENV_INTERNAL_TLS_CERT]: LEAF, [tlsconfig.ENV_INTERNAL_TLS_KEY]: KEY });
    expect(plain.clientCA).toBeUndefined();
  });

  it.each([
    [{ [tlsconfig.ENV_INTERNAL_TLS_CA]: "/nonexistent/ca.pem" }, /W17_INTERNAL_TLS_CA/],
    [{ [tlsconfig.ENV_INTERNAL_TLS_CA]: KEY }, /W17_INTERNAL_TLS_CA/],
    [{ [tlsconfig.ENV_INTERNAL_TLS_CERT]: LEAF }, /not both set/],
    [{ [tlsconfig.ENV_INTERNAL_TLS_KEY]: KEY }, /not both set/],
    [{ [tlsconfig.ENV_INTERNAL_TLS_CERT]: LEAF, [tlsconfig.ENV_INTERNAL_TLS_KEY]: "/nonexistent/key" }, /W17_INTERNAL_TLS_CERT/],
    [{ [tlsconfig.ENV_INTERNAL_TLS_CERT]: CA, [tlsconfig.ENV_INTERNAL_TLS_KEY]: KEY }, /W17_INTERNAL_TLS_CERT/],
  ])("a client refuses a broken config (%#)", (extra, match) => {
    expect(() => tlsconfig.clientTLS({ ...ON, ...extra })).toThrow(match);
  });

  it("a server refuses an unreadable client CA", () => {
    for (const bad of ["/nonexistent/ca.pem", KEY]) {
      expect(() =>
        tlsconfig.serverTLS({ ...ON, [tlsconfig.ENV_INTERNAL_TLS_CERT]: LEAF, [tlsconfig.ENV_INTERNAL_TLS_KEY]: KEY, [tlsconfig.ENV_INTERNAL_TLS_CLIENT_CA]: bad }),
      ).toThrow(/W17_INTERNAL_TLS_CLIENT_CA/);
    }
  });

  it("names the leaf for the health probe", () => {
    expect(tlsconfig.leafServerName(LEAF)).toBe("biz.test");
    expect(tlsconfig.leafServerName(CA)).toBeUndefined();
    expect(tlsconfig.leafServerName("/nonexistent")).toBeUndefined();
  });

  it("mutual TLS end to end: the tier, the probe, and a client without a pair", () =>
    inTmpDir(async () => {
      // The tier presents the leaf and requires a client cert from the CA; the
      // probe verifies the leaf against that CA alone by its DNS name and
      // presents the same leaf.
      quiet();
      const tls = { ...ON, W17_INTERNAL_TLS_CERT: LEAF, W17_INTERNAL_TLS_KEY: KEY, W17_INTERNAL_TLS_CA: CA, W17_INTERNAL_TLS_CLIENT_CA: CA };
      const app = new App(load(MANIFEST), { projectRoot: PROJECT, environ: env(tls) });
      await app.boot();
      await app.start();
      try {
        await probe(`127.0.0.1:${app.port}`, tls, 3000);
        // Without the client pair the server refuses the handshake.
        const bare = tlsconfig.channelCredentials(tlsconfig.clientTLS({ ...ON, W17_INTERNAL_TLS_CA: CA }));
        await expect(
          health.check(`127.0.0.1:${app.port}`, bare, 2000, { "grpc.ssl_target_name_override": "biz.test", "grpc.default_authority": "biz.test" }),
        ).rejects.toThrow();
      } finally {
        await app.shutdown();
      }
    }));
});

// --- Sentry and OpenTelemetry --------------------------------------------------

describe("telemetry", () => {
  it("Sentry sends no local variables, no PII, and registers no tracer provider", async () => {
    const Sentry = await import("@sentry/node");
    const obs = await observability.init("svc", { environ: { W17_SENTRY_DSN: "https://k@sentry.invalid/1" } });
    try {
      const opts = vi.mocked(Sentry.init).mock.calls.at(-1)![0]!;
      expect(opts.includeLocalVariables).toBe(false);
      expect(opts.enableOpenTelemetrySetup).toBe(false);
      expect(opts.dataCollection?.userInfo).toBe(false);
      expect(opts.serverName).toBe("svc");
      expect(opts.tracesSampleRate).toBe(0.2);
    } finally {
      await obs.shutdown();
    }
  });

  it("the Sentry scrub redacts credentials and secret fields", () => {
    const event = {
      request: {
        headers: { Authorization: "Bearer x", "x-api-key": "k", accept: "*/*" },
        data: { NOTES_API_TOKEN: "t0ken", q: "token NOTES_API_TOKEN=t0ken here" },
      },
      exception: { values: [{ stacktrace: { frames: [{ vars: { NOTES_API_TOKEN: "t0ken", x: 1 } }] } }] },
    };
    const out = observability.scrubEvent(event, ["NOTES_API_TOKEN"]);
    expect(out.request.headers.Authorization).toBe("[scrubbed]");
    expect(out.request.headers["x-api-key"]).toBe("[scrubbed]");
    expect(out.request.headers.accept).toBe("*/*");
    expect(JSON.stringify(out)).not.toContain("t0ken");
  });

  it("the scrub reaches exception values and messages", () => {
    const event = {
      exception: { values: [{ type: "Error", value: "dial failed: NOTES_API_TOKEN=t0ken, retry" }] },
      message: "boot: NOTES_API_TOKEN: 't0ken'",
      logentry: { message: "with NOTES_API_TOKEN=%s", formatted: "with NOTES_API_TOKEN=t0ken", params: ["NOTES_API_TOKEN=t0ken"] },
    };
    const out = observability.scrubEvent(event, ["NOTES_API_TOKEN"]);
    expect(JSON.stringify(out)).not.toContain("t0ken");
    expect(out.exception.values[0]!.value).toBe("dial failed: NOTES_API_TOKEN=[scrubbed], retry");
  });

  it("OTel init refuses a bad TLS value", async () => {
    await expect(observability.init("svc", { environ: { W17_OTEL_ENDPOINT: "otel:4317", W17_OTEL_TLS: "maybe" } })).rejects.toThrow(/W17_OTEL_TLS/);
  });

  it("OTel propagates traceparent when on, through the explicit context", async () => {
    const obs = await observability.init("svc", { environ: { W17_OTEL_ENDPOINT: "127.0.0.1:1" } });
    try {
      const api = observability.otelApi()!;
      const tp = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01";
      const ctx = new Context({ incoming: { traceparent: tp } });
      const parent = observability.extractTrace(ctx)!;
      const span = observability.tracer()!.startSpan("x", {}, parent);
      const traced = observability.withTrace(ctx, api.trace.setSpan(parent, span));
      const pairs = Object.fromEntries(observability.injectTrace(traced, [["a", "b"]]));
      span.end();
      expect(pairs.a).toBe("b");
      expect(pairs.traceparent!.split("-")[1]).toBe("0af7651916cd43dd8448eb211c80319c");
      // A context without the server's span injects nothing.
      expect(observability.injectTrace(new Context(), [["a", "b"]])).toEqual([["a", "b"]]);
    } finally {
      await obs.shutdown(100);
    }
    expect(observability.injectTrace(new Context(), [["a", "b"]])).toEqual([["a", "b"]]); // off again
  });
});
