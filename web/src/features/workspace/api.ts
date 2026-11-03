import { ApiError, getJson, post } from "@/lib/api";

export interface Workspace {
  workspaceId: string;
  name: string;
}

export interface UsageOptions {
  providers: string[];
  models: string[];
}

/** Go strings.TrimSpace 使用的 Unicode 空白集合（与 JS trim 不完全相同）。 */
const GO_SPACE =
  "\\t\\n\\v\\f\\r \\u0085\\u00A0\\u1680\\u2000-\\u200A\\u2028\\u2029\\u202F\\u205F\\u3000";
const LEADING_SPACE = new RegExp(`^[${GO_SPACE}]+`);
const TRAILING_SPACE = new RegExp(`[${GO_SPACE}]+$`);
const CONTROL = /[\u0000-\u001F\u007F-\u009F]/;
const NAME_MAX_CODE_POINTS = 80;
const NAME_MAX_BYTES = 256;

/** 与服务端一致的修剪；不做 Unicode 规范化或大小写折叠。 */
export function trimWorkspaceName(value: string): string {
  return value.replace(LEADING_SPACE, "").replace(TRAILING_SPACE, "");
}

/** 即时校验，服务端仍是唯一权威；合法返回 null。 */
export function workspaceNameProblem(raw: string): string | null {
  const name = trimWorkspaceName(raw);
  if (name === "") return "请输入工作区名称";
  if (CONTROL.test(name)) return "名称不能包含控制字符";
  if (Array.from(name).length > NAME_MAX_CODE_POINTS) {
    return `名称最多 ${NAME_MAX_CODE_POINTS} 个字符`;
  }
  if (new TextEncoder().encode(name).length > NAME_MAX_BYTES) {
    return `名称最多 ${NAME_MAX_BYTES} 字节（UTF-8）`;
  }
  return null;
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

/** 把未知响应体收窄为工作区；空 ID 或非字符串名称按数据错误处理。 */
export function parseWorkspace(body: unknown): Workspace {
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

/** 列表顶层必须是数组；服务端顺序原样保留，不在此重排。 */
export async function fetchWorkspaces(
  signal: AbortSignal,
): Promise<Workspace[]> {
  const body = await getJson("/api/v1/workspaces", signal);
  if (!Array.isArray(body)) throw badResponse();
  return body.map(parseWorkspace);
}

/** 切换前验证目标可访问；非成员由服务端统一返回 403。 */
export async function fetchWorkspace(
  workspaceId: string,
  signal: AbortSignal,
): Promise<Workspace> {
  const body = await getJson(
    `/api/v1/workspaces/${encodeURIComponent(workspaceId)}`,
    signal,
  );
  return parseWorkspace(body);
}

/** 创建工作区；名称由调用方先按服务端规则修剪，响应返回修剪后的名称。 */
export async function createWorkspace(name: string): Promise<Workspace> {
  const body = await post("/api/v1/workspaces", { name });
  return parseWorkspace(body);
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
