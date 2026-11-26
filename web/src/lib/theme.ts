import { useSyncExternalStore } from "react";

export type ThemeId = "editorial-light" | "graphite-dark";

export const DEFAULT_THEME: ThemeId = "editorial-light";
const STORAGE_KEY = "tokenpivot.theme";
const listeners = new Set<() => void>();

function isTheme(value: string | null): value is ThemeId {
  return value === "editorial-light" || value === "graphite-dark";
}

function applyTheme(theme: ThemeId): void {
  if (typeof document !== "undefined") {
    document.documentElement.dataset.theme = theme;
  }
}

function readTheme(): ThemeId {
  if (typeof document !== "undefined") {
    const attr = document.documentElement.dataset.theme ?? null;
    if (isTheme(attr)) return attr;
  }
  try {
    const stored = window.localStorage.getItem(STORAGE_KEY);
    if (isTheme(stored)) return stored;
  } catch {
    // 隐私模式等场景下回退默认主题。
  }
  return DEFAULT_THEME;
}

let currentTheme: ThemeId = readTheme();
applyTheme(currentTheme);

export function setTheme(next: ThemeId): void {
  if (next === currentTheme) return;
  currentTheme = next;
  applyTheme(next);
  try {
    window.localStorage.setItem(STORAGE_KEY, next);
  } catch {
    // 无法持久化时仅本次会话生效。
  }
  listeners.forEach((listener) => listener());
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

/** 当前界面主题：默认暖白（editorial-light），支持切换石墨暗（graphite-dark）。 */
export function useTheme(): ThemeId {
  return useSyncExternalStore(
    subscribe,
    () => currentTheme,
    () => DEFAULT_THEME,
  );
}
