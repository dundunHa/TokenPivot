import { useSyncExternalStore } from "react";

/** 概览页可选的自动刷新间隔（毫秒）。 */
export const REFRESH_CHOICES = [1_000, 5_000, 10_000] as const;
export type RefreshMs = (typeof REFRESH_CHOICES)[number];
export const DEFAULT_REFRESH_MS: RefreshMs = 5_000;

const STORAGE_KEY = "tokenpivot.overview.refreshMs";
const listeners = new Set<() => void>();

function isChoice(value: number): value is RefreshMs {
  return (REFRESH_CHOICES as readonly number[]).includes(value);
}

function read(): RefreshMs {
  try {
    const stored = Number(window.localStorage.getItem(STORAGE_KEY));
    if (isChoice(stored)) return stored;
  } catch {
    // 存储不可用（隐私模式等）时退回默认值。
  }
  return DEFAULT_REFRESH_MS;
}

let current: RefreshMs = read();

export function setRefreshMs(next: RefreshMs): void {
  if (next === current) return;
  current = next;
  try {
    window.localStorage.setItem(STORAGE_KEY, String(next));
  } catch {
    // 无法持久化时仅本次会话生效。
  }
  listeners.forEach((l) => l());
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

/** 读取当前选择的刷新间隔；偏好保存在浏览器本地，仅影响概览页。 */
export function useRefreshMs(): RefreshMs {
  return useSyncExternalStore(
    subscribe,
    () => current,
    () => DEFAULT_REFRESH_MS,
  );
}

export function refreshLabel(ms: RefreshMs): string {
  return `${ms / 1000} 秒`;
}
