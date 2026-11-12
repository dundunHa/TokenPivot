import { describe, it, expect } from "vitest";
import { brailleRender, fmtCompact } from "./braille";

describe("brailleRender", () => {
  it("returns correct number of lines", () => {
    const result = brailleRender([{ values: [1, 2, 3, 4, 5], label: "test" }], {
      cols: 20,
      rows: 4,
    });
    expect(result.lines).toHaveLength(4);
  });

  it("each line has correct character count", () => {
    const result = brailleRender([{ values: [1, 2, 3], label: "test" }], {
      cols: 10,
      rows: 2,
    });
    for (const line of result.lines) {
      expect([...line].length).toBe(10);
    }
  });

  it("returns axis and legend lines", () => {
    const result = brailleRender([{ values: [1, 2, 3], label: "input" }], {
      cols: 20,
      rows: 4,
    });
    expect(result.axisLine).toBeTruthy();
    expect(result.legendLine).toContain("input");
  });

  it("handles single series with primary flag", () => {
    const result = brailleRender(
      [{ values: [10, 20, 30, 20, 10], label: "main", primary: true }],
      { cols: 20, rows: 4 },
    );
    // Primary series should produce non-empty braille characters
    const hasContent = result.lines.some((line) =>
      [...line].some((ch) => ch.charCodeAt(0) > 0x2800),
    );
    expect(hasContent).toBe(true);
  });

  it("handles empty values gracefully", () => {
    const result = brailleRender([{ values: [], label: "empty" }], {
      cols: 10,
      rows: 2,
    });
    expect(result.lines).toHaveLength(2);
  });

  it("handles constant values (min === max)", () => {
    const result = brailleRender([{ values: [5, 5, 5, 5], label: "flat" }], {
      cols: 10,
      rows: 2,
    });
    expect(result.lines).toHaveLength(2);
    expect(result.yRange.max).toBeGreaterThan(result.yRange.min);
  });

  it("multi-series renders both", () => {
    const result = brailleRender(
      [
        { values: [1, 2, 3, 4], label: "input", primary: true },
        { values: [4, 3, 2, 1], label: "output" },
      ],
      { cols: 20, rows: 4 },
    );
    expect(result.legendLine).toContain("input");
    expect(result.legendLine).toContain("output");
  });
});

describe("fmtCompact", () => {
  it("formats billions", () => {
    expect(fmtCompact(1_500_000_000)).toBe("1.50B");
  });
  it("formats millions", () => {
    expect(fmtCompact(37_980_000)).toBe("37.98M");
  });
  it("formats thousands", () => {
    expect(fmtCompact(4_200)).toBe("4.2K");
  });
  it("formats small numbers", () => {
    expect(fmtCompact(42)).toBe("42");
  });
});
