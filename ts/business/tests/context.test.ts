// The request context: cancellation, deadlines and what reads them.

import { afterEach, describe, expect, it, vi } from "vitest";
import { Context } from "../src/context.ts";
import { sleep } from "./helpers.ts";

afterEach(() => vi.restoreAllMocks());

describe("onDone", () => {
  it("a callback that throws is reported, never thrown into the canceller", async () => {
    const logged: string[] = [];
    vi.spyOn(console, "error").mockImplementation((s: string) => void logged.push(s));
    const ctx = new Context().withCancel();
    const ran: number[] = [];
    ctx.onDone(() => {
      throw new Error("author bug");
    });
    ctx.onDone(() => void ran.push(1));
    // In a server the canceller is a grpc-js 'cancelled' listener: a throw
    // there is an uncaught exception that ends the process.
    expect(() => ctx.cancel()).not.toThrow();
    expect(ran).toEqual([1]);
    expect(logged.some((l) => l.includes("onDone callback threw") && l.includes("author bug"))).toBe(true);

    // The same on the deadline's timer and on an already-done context.
    logged.length = 0;
    const timed = new Context().withTimeout(5);
    timed.onDone(() => {
      throw new Error("on the timer");
    });
    await sleep(30);
    timed.onDone(() => {
      throw new Error("on the microtask");
    });
    await sleep(5);
    expect(logged.filter((l) => l.includes("onDone callback threw")).length).toBe(2);
  });
});

describe("err", () => {
  it("a deadline that had passed when the context was cancelled is the reason", async () => {
    const ctx = new Context().withTimeout(5);
    await sleep(20);
    ctx.cancel(); // as the server's 'cancelled' after DEADLINE_EXCEEDED
    expect(ctx.err()).toBe("context deadline exceeded");
    expect(ctx.withValue("k", 1).err()).toBe("context deadline exceeded");
  });

  it("a cancel before the deadline stays a cancel after it passes, as Go keeps the first", async () => {
    const ctx = new Context().withTimeout(10);
    ctx.cancel();
    await sleep(25);
    expect(ctx.err()).toBe("context canceled");
  });

  it("a parent's deadline reaches a child cancelled with it", async () => {
    const parent = new Context().withTimeout(5);
    const child = parent.withCancel();
    await sleep(20);
    parent.cancel();
    expect(child.err()).toBe("context deadline exceeded");
  });
});
