import { createHash } from "crypto";

export function redactForLog(value?: string | null): string {
  if (!value) return "none";
  return createHash("sha256").update(value).digest("hex").slice(0, 12);
}
