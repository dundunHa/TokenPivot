import { describe, expect, it } from "vitest";
import { fmtAxis, fmtRate, niceTicks } from "./chartScale";

describe("niceTicks", () => {
  it("rounds the maximum up to a readable step starting at zero", () => {
    expect(niceTicks(340_000)).toEqual({
      ticks: [0, 100_000, 200_000, 300_000, 400_000],
      max: 400_000,
    });
    expect(niceTicks(97)).toEqual({ ticks: [0, 25, 50, 75, 100], max: 100 });
    expect(niceTicks(9)).toEqual({ ticks: [0, 2.5, 5, 7.5, 10], max: 10 });
  });

  it("falls back to a unit scale for empty or invalid maxima", () => {
    expect(niceTicks(0)).toEqual({ ticks: [0, 1], max: 1 });
    expect(niceTicks(Number.NaN)).toEqual({ ticks: [0, 1], max: 1 });
  });

  it("covers the maximum exactly at a step boundary", () => {
    const r = niceTicks(400);
    expect(r.max).toBeGreaterThanOrEqual(400);
    expect(r.ticks[0]).toBe(0);
  });
});

describe("axis formatting", () => {
  it("drops trailing .0 and keeps units", () => {
    expect(fmtAxis(340_000)).toBe("340K");
    expect(fmtAxis(1_500_000)).toBe("1.5M");
    expect(fmtAxis(85)).toBe("85");
    expect(fmtAxis(2_500)).toBe("2.5K");
  });

  it("formats rates by magnitude", () => {
    expect(fmtRate(803.4)).toBe("803");
    expect(fmtRate(80.34)).toBe("80.3");
    expect(fmtRate(0.04)).toBe("0");
    expect(fmtRate(1250)).toBe("1.3K");
  });
});
