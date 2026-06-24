// Tests for clientLog throttling + global-handler idempotency.
//
// The Wails binding is mocked: clientLog forwards to OctoBuddyService.
// LogClientError, which returns a Promise the real Wails runtime resolves
// when the Go side ack's. Under test we never load the runtime — the
// mock just records calls.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// vi.mock must run BEFORE the SUT import — Vitest hoists this. The mock
// must mirror the binding's named export shape; clientLog imports
// `OctoBuddyService` and calls `.LogClientError(...)`. Path is written
// from the SUT's perspective (src/lib/clientLog.ts) — Vitest resolves to
// the absolute module ID and matches against any importer.
const mockLogClientError = vi.fn(() => Promise.resolve());
vi.mock("../bindings/github.com/lml2468/octobuddy/desktop", () => ({
  OctoBuddyService: {
    LogClientError: mockLogClientError,
  },
}));

// Reset module state between tests so the throttler's lastSeen map and
// installGlobalErrorCapture's once-flag don't leak across cases.
beforeEach(() => {
  vi.resetModules();
  mockLogClientError.mockClear();
});

afterEach(() => {
  vi.useRealTimers();
});

describe("clientLog throttling", () => {
  it("forwards a single call to the binding", async () => {
    const { clientLog } = await import("../src/lib/clientLog");
    clientLog("test", "first", "stack here");
    expect(mockLogClientError).toHaveBeenCalledTimes(1);
    expect(mockLogClientError).toHaveBeenCalledWith("test", "first", "stack here");
  });

  it("dedupes identical (category, message) within the throttle window", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-06-24T12:00:00Z"));
    const { clientLog } = await import("../src/lib/clientLog");

    clientLog("burst", "same message");
    clientLog("burst", "same message");
    clientLog("burst", "same message");

    expect(mockLogClientError).toHaveBeenCalledTimes(1);
  });

  it("emits again once the throttle window expires (5s)", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-06-24T12:00:00Z"));
    const { clientLog } = await import("../src/lib/clientLog");

    clientLog("burst", "same message");
    vi.advanceTimersByTime(5_001);
    clientLog("burst", "same message");

    expect(mockLogClientError).toHaveBeenCalledTimes(2);
  });

  it("doesn't dedupe across different categories", async () => {
    const { clientLog } = await import("../src/lib/clientLog");
    clientLog("a", "same body");
    clientLog("b", "same body");
    expect(mockLogClientError).toHaveBeenCalledTimes(2);
  });

  it("clips an oversized message to the field cap with a marker", async () => {
    const { clientLog } = await import("../src/lib/clientLog");
    const huge = "x".repeat(100_000);
    clientLog("oversize", huge);

    expect(mockLogClientError).toHaveBeenCalledTimes(1);
    const args = mockLogClientError.mock.calls[0] as unknown as [string, string, string];
    expect(args[1].length).toBeLessThanOrEqual(8 * 1024 + "…(clipped)".length);
    expect(args[1].endsWith("…(clipped)")).toBe(true);
  });

  it("swallows IPC failures so a failed log doesn't cascade", async () => {
    mockLogClientError.mockImplementationOnce(() => Promise.reject(new Error("ipc down")));
    const { clientLog } = await import("../src/lib/clientLog");
    // No throw expected; an unhandled rejection here would crash the
    // global error handler that called clientLog.
    expect(() => clientLog("ipc", "test")).not.toThrow();
    // Await microtask flush so the .catch handler runs before assertions.
    await new Promise((r) => setTimeout(r, 0));
  });
});

describe("installGlobalErrorCapture", () => {
  it("is idempotent across repeated calls (HMR-safe)", async () => {
    const { installGlobalErrorCapture, clientLog } = await import("../src/lib/clientLog");
    const addSpy = vi.spyOn(window, "addEventListener");

    installGlobalErrorCapture();
    installGlobalErrorCapture();
    installGlobalErrorCapture();

    // First call registers "error" + "unhandledrejection" → 2 listeners
    // total. Subsequent calls must NOT stack on top.
    const errorListenerCount = addSpy.mock.calls.filter((c) => c[0] === "error").length;
    const rejectionListenerCount = addSpy.mock.calls.filter((c) => c[0] === "unhandledrejection").length;
    expect(errorListenerCount).toBe(1);
    expect(rejectionListenerCount).toBe(1);

    // And clientLog still works after install.
    clientLog("installed", "yes");
    expect(mockLogClientError).toHaveBeenCalled();

    addSpy.mockRestore();
  });
});
