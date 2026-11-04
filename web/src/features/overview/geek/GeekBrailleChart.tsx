import { useMemo } from "react";
import { brailleRender } from "./braille";
import type { TrendModel } from "../overviewView";

interface GeekBrailleChartProps {
  trend: TrendModel;
  /** 字符列数 */
  cols?: number;
  /** 字符行数 */
  rows?: number;
}

/**
 * J 风格 braille 4×2 点阵真曲线图。
 * 缺口（gap）映射为 0 值断段；null 映射为 0 保持连续但不虚报。
 */
export function GeekBrailleChart({
  trend,
  cols = 88,
  rows = 10,
}: GeekBrailleChartProps) {
  const result = useMemo(() => {
    if (trend.points.length === 0) return null;

    // 保留所有点（含 gap），null→0 表示断段
    const inputValues = trend.points.map((p) => (p.gap ? 0 : (p.input ?? 0)));
    const outputValues = trend.points.map((p) => (p.gap ? 0 : (p.output ?? 0)));

    // 检查是否有 partial 桶（未完成时段）
    const hasPartial = trend.points.some((p) => p.partial);

    return brailleRender(
      [
        {
          values: inputValues,
          label: "input",
          primary: true,
        },
        {
          values: outputValues,
          label: "output",
        },
      ],
      { cols, rows, markCurrent: hasPartial },
    );
  }, [trend.points, cols, rows]);

  if (!result) {
    return <div className="geek-empty geek-dim">无趋势数据。</div>;
  }

  return (
    <div className="geek-braille">
      {result.lines.map((line, i) => (
        <div key={i} className="geek-braille-line">
          {line}
        </div>
      ))}
      <div className="geek-braille-axis geek-dim">{result.axisLine}</div>
      <div className="geek-braille-legend geek-dim">{result.legendLine}</div>
      <div className="geek-braille-note geek-dim">
        ▸ 按完成时刻计数 · 补报回填发生时段不算新消耗
        {trend.stackNote ? ` · ${trend.stackNote}` : ""}
      </div>
    </div>
  );
}
