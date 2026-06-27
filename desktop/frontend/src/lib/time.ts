// formatMsgTime renders a Unix-seconds timestamp as a chat-style time label,
// WeChat/iMessage convention: today → "HH:MM", yesterday → "昨天 HH:MM", this
// year → "M月D日 HH:MM", older → "YYYY/M/D HH:MM". Returns "" for a missing /
// zero / unparseable ts (e.g. preview rows seeded with ts:0) so the caller can
// `{#if}` it away rather than printing a bogus "1970" stamp.
export function formatMsgTime(ts: number): string {
  if (!ts) return "";
  const d = new Date(ts * 1000);
  if (Number.isNaN(d.getTime())) return "";
  const now = new Date();
  const hm = `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
  const sameDay = (a: Date, b: Date) =>
    a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
  if (sameDay(d, now)) return hm;
  const yest = new Date(now);
  yest.setDate(now.getDate() - 1);
  if (sameDay(d, yest)) return `昨天 ${hm}`;
  if (d.getFullYear() === now.getFullYear()) return `${d.getMonth() + 1}月${d.getDate()}日 ${hm}`;
  return `${d.getFullYear()}/${d.getMonth() + 1}/${d.getDate()} ${hm}`;
}
