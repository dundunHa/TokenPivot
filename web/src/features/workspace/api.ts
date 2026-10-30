import { ApiError, getJson } from "@/lib/api";

export interface CurrentWorkspace {
  workspaceId: string;
  name: string;
}

export interface UsageOptions {
  providers: string[];
  models: string[];
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function isStringList(value: unknown): value is string[] {
  return Array.isArray(value) && value.every((v) => typeof v === "string");
}

function badResponse(): ApiError {
  return new ApiError(200, "BAD_RESPONSE");
}

/** 当前用户的个人空间；形状不合法按数据错误处理，不当作空值。 */
export async function fetchCurrentWorkspace(
  signal: AbortSignal,
): Promise<CurrentWorkspace> {
  const body = await getJson("/api/v1/workspaces/current", signal);
  if (
    !isRecord(body) ||
    typeof body.workspace_id !== "string" ||
    body.workspace_id === "" ||
    typeof body.name !== "string"
  ) {
    throw badResponse();
  }
  return { workspaceId: body.workspace_id, name: body.name };
}

/** Provider / Model 筛选选项；空数组是合法结果，缺字段不是。 */
export async function fetchUsageOptions(
  workspaceId: string,
  signal: AbortSignal,
): Promise<UsageOptions> {
  const body = await getJson(
    `/api/v1/workspaces/${encodeURIComponent(workspaceId)}/usage/options`,
    signal,
  );
  if (
    !isRecord(body) ||
    !isStringList(body.providers) ||
    !isStringList(body.models)
  ) {
    throw badResponse();
  }
  return { providers: body.providers, models: body.models };
}
