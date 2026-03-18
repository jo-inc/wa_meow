import { describe, expect, it } from "vitest";
import { redactForLog } from "./redaction.js";

describe("redactForLog", () => {
  it("returns a stable short hash for non-empty values", () => {
    const first = redactForLog("1234567890@s.whatsapp.net");
    const second = redactForLog("1234567890@s.whatsapp.net");

    expect(first).toBe(second);
    expect(first).not.toBe("1234567890@s.whatsapp.net");
    expect(first).toHaveLength(12);
  });

  it("returns none for empty values", () => {
    expect(redactForLog("")).toBe("none");
    expect(redactForLog(undefined)).toBe("none");
    expect(redactForLog(null)).toBe("none");
  });
});
