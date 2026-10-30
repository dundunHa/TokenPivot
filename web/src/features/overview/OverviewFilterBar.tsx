import { useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  GRANULARITIES,
  validateCustomRange,
  type Granularity,
  type OverviewFilters,
  type RangePreset,
} from "./overviewFilters";

const PRESETS: { id: RangePreset; label: string }[] = [
  { id: "1h", label: "最近一小时" },
  { id: "today", label: "今天" },
  { id: "yesterday", label: "昨天" },
  { id: "7d", label: "最近七天" },
  { id: "custom", label: "自定义" },
];
const GRAN_LABEL: Record<Granularity, string> = {
  minute: "分钟",
  hour: "小时",
  day: "天",
};

export interface Option {
  value: string;
  label: string;
}

function MultiSelect({
  label,
  options,
  selected,
  disabledHint,
  onChange,
}: {
  label: string;
  options: Option[];
  selected: string[];
  disabledHint: string | null;
  onChange: (next: string[]) => void;
}) {
  const known = new Set(options.map((o) => o.value));
  // URL 中有效但选项里不存在的标识不丢弃，原样列出。
  const all = [
    ...options,
    ...selected
      .filter((v) => !known.has(v))
      .map((v) => ({ value: v, label: v })),
  ];
  return (
    <details className="border-border rounded-md border">
      <summary className="cursor-pointer px-3 py-1 text-sm">
        {label}
        {selected.length > 0 ? `（${selected.length}）` : "（全部）"}
      </summary>
      <div className="flex max-h-56 flex-col gap-1 overflow-auto px-3 py-2">
        {disabledHint ? (
          <p role="alert" className="text-destructive text-xs">
            {disabledHint}
          </p>
        ) : null}
        {all.length === 0 && !disabledHint ? (
          <p className="text-muted-foreground text-xs">暂无选项</p>
        ) : null}
        {all.map((o) => (
          <label key={o.value} className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={selected.includes(o.value)}
              disabled={disabledHint !== null}
              onChange={(e) =>
                onChange(
                  e.target.checked
                    ? [...selected, o.value]
                    : selected.filter((v) => v !== o.value),
                )
              }
            />
            {o.label}
          </label>
        ))}
      </div>
    </details>
  );
}

export function OverviewFilterBar({
  filters,
  providerOptions,
  modelOptions,
  projectOptions,
  optionsError,
  projectsError,
  onChange,
}: {
  filters: OverviewFilters;
  providerOptions: Option[];
  modelOptions: Option[];
  projectOptions: Option[];
  optionsError: string | null;
  projectsError: string | null;
  onChange: (next: OverviewFilters) => void;
}) {
  const [customOpen, setCustomOpen] = useState(filters.range === "custom");
  const [draftStart, setDraftStart] = useState(filters.start ?? "");
  const [draftEnd, setDraftEnd] = useState(filters.end ?? "");
  const [customError, setCustomError] = useState<string | null>(null);

  const names = new Map(projectOptions.map((o) => [o.value, o.label]));
  const chips: { key: string; text: string; remove: () => void }[] = [
    ...filters.providers.map((v) => ({
      key: `p:${v}`,
      text: `供应商 ${v}`,
      remove: () =>
        onChange({
          ...filters,
          providers: filters.providers.filter((x) => x !== v),
        }),
    })),
    ...filters.models.map((v) => ({
      key: `m:${v}`,
      text: `模型 ${v}`,
      remove: () =>
        onChange({ ...filters, models: filters.models.filter((x) => x !== v) }),
    })),
    ...filters.projects.map((v) => ({
      key: `j:${v}`,
      text: `项目 ${v === "unassigned" ? "未归属" : (names.get(v) ?? v)}`,
      remove: () =>
        onChange({
          ...filters,
          projects: filters.projects.filter((x) => x !== v),
        }),
    })),
  ];

  function pickPreset(id: RangePreset) {
    if (id === "custom") {
      setCustomOpen(true);
      return;
    }
    setCustomOpen(false);
    setCustomError(null);
    onChange({ ...filters, range: id, start: null, end: null });
  }

  function applyCustom() {
    const problem = validateCustomRange(draftStart, draftEnd, filters.timezone);
    setCustomError(problem);
    if (problem) return;
    onChange({ ...filters, range: "custom", start: draftStart, end: draftEnd });
  }

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-start gap-2">
        <MultiSelect
          label="供应商"
          options={providerOptions}
          selected={filters.providers}
          disabledHint={optionsError}
          onChange={(providers) => onChange({ ...filters, providers })}
        />
        <MultiSelect
          label="模型"
          options={modelOptions}
          selected={filters.models}
          disabledHint={optionsError}
          onChange={(models) => onChange({ ...filters, models })}
        />
        <MultiSelect
          label="项目"
          options={projectOptions}
          selected={filters.projects}
          disabledHint={projectsError}
          onChange={(projects) => onChange({ ...filters, projects })}
        />
      </div>
      {chips.length > 0 ? (
        <ul aria-label="已选筛选" className="flex flex-wrap gap-2">
          {chips.map((c) => (
            <li key={c.key}>
              <button
                type="button"
                aria-label={`移除 ${c.text}`}
                onClick={c.remove}
                className="border-border rounded-md border px-2 py-0.5 text-xs"
              >
                {c.text} ×
              </button>
            </li>
          ))}
        </ul>
      ) : null}
      <div role="group" aria-label="时间范围" className="flex flex-wrap gap-2">
        {PRESETS.map((p) => (
          <button
            key={p.id}
            type="button"
            aria-pressed={
              p.id === "custom"
                ? customOpen
                : !customOpen && filters.range === p.id
            }
            onClick={() => pickPreset(p.id)}
            className="border-border aria-pressed:border-primary aria-pressed:text-primary rounded-md border px-3 py-1 text-sm"
          >
            {p.label}
          </button>
        ))}
        <span className="mx-1" />
        <div role="group" aria-label="粒度" className="flex gap-2">
          {GRANULARITIES.map((g) => (
            <button
              key={g}
              type="button"
              aria-pressed={filters.granularity === g}
              onClick={() => onChange({ ...filters, granularity: g })}
              className="border-border aria-pressed:border-primary aria-pressed:text-primary rounded-md border px-3 py-1 text-sm"
            >
              {GRAN_LABEL[g]}
            </button>
          ))}
        </div>
      </div>
      {customOpen ? (
        <div className="flex flex-col gap-2">
          <div className="flex flex-wrap items-end gap-2">
            <label className="flex flex-col gap-1 text-xs">
              起点
              <Input
                value={draftStart}
                placeholder="2026-10-28T09:00:00+08:00"
                onChange={(e) => setDraftStart(e.target.value)}
              />
            </label>
            <label className="flex flex-col gap-1 text-xs">
              终点
              <Input
                value={draftEnd}
                placeholder="2026-10-28T18:00:00+08:00"
                onChange={(e) => setDraftEnd(e.target.value)}
              />
            </label>
            <Button variant="outline" onClick={applyCustom}>
              应用
            </Button>
          </div>
          {customError ? (
            <p role="alert" className="text-destructive text-xs">
              {customError}
            </p>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}
