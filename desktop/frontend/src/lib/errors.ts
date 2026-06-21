// errMsg coerces an unknown caught value into a human-readable message.
// Wails Go-binding rejections arrive as plain Error objects but third-party
// callers (postMessage, browser APIs) may throw strings, numbers, or
// {message: "..."} bags — this normalizes all of them. Centralized so we can
// upgrade the formatting (translation, stack trim) without grepping for
// `catch (e: any)` again.
export function errMsg(e: unknown): string {
  if (e instanceof Error) return e.message;
  if (typeof e === "string") return e;
  if (e && typeof e === "object" && "message" in e) {
    const m = (e as { message?: unknown }).message;
    if (typeof m === "string") return m;
  }
  return String(e);
}
