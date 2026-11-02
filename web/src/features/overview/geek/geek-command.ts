/**
 * geek-command.ts — 命令行过滤解析
 *
 * 解析 `provider=openai model=gpt project=tokenpivot` 风格的命令输入，
 * 更新 OverviewFilters。
 */

import type { OverviewFilters } from "../overviewFilters";

export interface ParsedCommand {
  /** 解析后的筛选更新（部分字段） */
  updates: Partial<OverviewFilters>;
  /** 解析错误信息（空 = 无错误） */
  errors: string[];
  /** 原始输入 */
  raw: string;
}

/**
 * 解析命令输入字符串为筛选更新。
 *
 * 支持格式：
 *   provider=openai
 *   model=gpt-5.2
 *   project=tokenpivot
 *   provider=openai,anthropic（逗号分隔多值）
 *
 * 空字符串或纯空格 = 清空所有筛选。
 */
export function parseCommand(input: string): ParsedCommand {
  const raw = input;
  const trimmed = input.trim();
  const errors: string[] = [];
  const updates: Partial<OverviewFilters> = {};

  if (!trimmed) {
    return { updates: { providers: [], models: [], projects: [] }, errors, raw };
  }

  const tokens = trimmed.split(/\s+/);
  for (const token of tokens) {
    const eqIdx = token.indexOf("=");
    if (eqIdx < 0) {
      errors.push(`无效格式: "${token}"（需要 key=value）`);
      continue;
    }

    const key = token.slice(0, eqIdx).toLowerCase();
    const value = token.slice(eqIdx + 1);

    if (!value) {
      errors.push(`空值: "${key}="`);
      continue;
    }

    const values = value.split(",").filter(Boolean);

    switch (key) {
      case "provider":
      case "p":
        updates.providers = values;
        break;
      case "model":
      case "m":
        updates.models = values;
        break;
      case "project":
      case "proj":
        updates.projects = values;
        break;
      default:
        errors.push(`未知键: "${key}"（支持 provider/model/project）`);
    }
  }

  return { updates, errors, raw };
}

/**
 * 将当前筛选状态序列化为命令字符串（用于回填输入框）。
 */
export function filtersToCommand(filters: OverviewFilters): string {
  const parts: string[] = [];
  if (filters.providers.length > 0) {
    parts.push(`provider=${filters.providers.join(",")}`);
  }
  if (filters.models.length > 0) {
    parts.push(`model=${filters.models.join(",")}`);
  }
  if (filters.projects.length > 0) {
    parts.push(`project=${filters.projects.join(",")}`);
  }
  return parts.join(" ");
}
