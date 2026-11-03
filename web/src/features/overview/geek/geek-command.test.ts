import { describe, it, expect } from "vitest";
import { parseCommand, filtersToCommand } from "./geek-command";
import { DEFAULT_OVERVIEW_FILTERS } from "../overviewFilters";

const TZ = "Asia/Shanghai";
const defaults = { ...DEFAULT_OVERVIEW_FILTERS, timezone: TZ };

describe("parseCommand", () => {
  it("parses single provider filter", () => {
    const result = parseCommand("provider=openai");
    expect(result.errors).toHaveLength(0);
    expect(result.updates.providers).toEqual(["openai"]);
  });

  it("parses multiple filters", () => {
    const result = parseCommand(
      "provider=openai model=gpt-5.2 project=tokenpivot",
    );
    expect(result.errors).toHaveLength(0);
    expect(result.updates.providers).toEqual(["openai"]);
    expect(result.updates.models).toEqual(["gpt-5.2"]);
    expect(result.updates.projects).toEqual(["tokenpivot"]);
  });

  it("parses comma-separated multi-values", () => {
    const result = parseCommand("provider=openai,anthropic");
    expect(result.errors).toHaveLength(0);
    expect(result.updates.providers).toEqual(["openai", "anthropic"]);
  });

  it("parses shorthand keys", () => {
    const result = parseCommand("p=openai m=gpt proj=tokenpivot");
    expect(result.errors).toHaveLength(0);
    expect(result.updates.providers).toEqual(["openai"]);
    expect(result.updates.models).toEqual(["gpt"]);
    expect(result.updates.projects).toEqual(["tokenpivot"]);
  });

  it("empty string clears all filters", () => {
    const result = parseCommand("");
    expect(result.errors).toHaveLength(0);
    expect(result.updates.providers).toEqual([]);
    expect(result.updates.models).toEqual([]);
    expect(result.updates.projects).toEqual([]);
  });

  it("reports error for invalid format", () => {
    const result = parseCommand("invalid");
    expect(result.errors).toHaveLength(1);
    expect(result.errors[0]).toContain("无效格式");
  });

  it("reports error for empty value", () => {
    const result = parseCommand("provider=");
    expect(result.errors).toHaveLength(1);
    expect(result.errors[0]).toContain("空值");
  });

  it("reports error for unknown key", () => {
    const result = parseCommand("unknown=value");
    expect(result.errors).toHaveLength(1);
    expect(result.errors[0]).toContain("未知键");
  });

  it("handles mixed valid and invalid tokens", () => {
    const result = parseCommand("provider=openai bad model=gpt");
    expect(result.errors).toHaveLength(1);
    expect(result.updates.providers).toEqual(["openai"]);
    expect(result.updates.models).toEqual(["gpt"]);
  });
});

describe("filtersToCommand", () => {
  it("serializes empty filters", () => {
    expect(filtersToCommand(defaults)).toBe("");
  });

  it("serializes all filters", () => {
    const cmd = filtersToCommand({
      ...defaults,
      providers: ["openai"],
      models: ["gpt-5.2"],
      projects: ["tokenpivot"],
    });
    expect(cmd).toBe("provider=openai model=gpt-5.2 project=tokenpivot");
  });

  it("roundtrip: parse(filtersToCommand(f)) preserves filters", () => {
    const original = {
      ...defaults,
      providers: ["openai", "anthropic"],
      models: ["gpt-5.2"],
      projects: [],
    };
    const cmd = filtersToCommand(original);
    const parsed = parseCommand(cmd);
    expect(parsed.updates.providers).toEqual(original.providers);
    expect(parsed.updates.models).toEqual(original.models);
  });
});
