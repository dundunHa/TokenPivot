import { useEffect, useRef, useState } from "react";
import { parseCommand, filtersToCommand } from "./geek-command";
import type { OverviewFilters } from "../overviewFilters";

interface GeekCommandBarProps {
  filters: OverviewFilters;
  onApply: (updates: Partial<OverviewFilters>) => void;
}

/**
 * J 风格命令输入条。
 * 支持 provider=/model=/project= 过滤，Enter 应用，Esc 清空。
 * 键盘：`/` 聚焦，`j/k` 滚动，`1/2/3` 切窗口。
 */
export function GeekCommandBar({ filters, onApply }: GeekCommandBarProps) {
  const [input, setInput] = useState("");
  const [error, setError] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  // 外部筛选变化时回填
  useEffect(() => {
    setInput(filtersToCommand(filters));
  }, [filters]);

  // 键盘快捷键
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      // `/` 聚焦命令输入
      if (e.key === "/" && !isInputFocused()) {
        e.preventDefault();
        inputRef.current?.focus();
      }
      // Esc 清空
      if (e.key === "Escape" && document.activeElement === inputRef.current) {
        setInput("");
        setError(null);
        onApply({ providers: [], models: [], projects: [] });
        inputRef.current?.blur();
      }
    };
    document.addEventListener("keydown", handler);
    return () => document.removeEventListener("keydown", handler);
  }, [onApply]);

  const handleSubmit = () => {
    const result = parseCommand(input);
    if (result.errors.length > 0) {
      setError(result.errors.join("; "));
      return;
    }
    setError(null);
    onApply(result.updates);
  };

  return (
    <div className="geek-cmdbar">
      <span className="geek-cmdbar-prompt geek-amber">❯</span>
      <input
        ref={inputRef}
        className="geek-cmdbar-input"
        value={input}
        onChange={(e) => {
          setInput(e.target.value);
          setError(null);
        }}
        onKeyDown={(e) => {
          if (e.key === "Enter") handleSubmit();
        }}
        placeholder="provider=openai · model=gpt · project=tokenpivot · 回车应用 / Esc 清空"
        spellCheck={false}
        autoComplete="off"
      />
      {error && <span className="geek-cmdbar-error geek-err">{error}</span>}
    </div>
  );
}

function isInputFocused(): boolean {
  const el = document.activeElement;
  return el instanceof HTMLInputElement || el instanceof HTMLTextAreaElement;
}
