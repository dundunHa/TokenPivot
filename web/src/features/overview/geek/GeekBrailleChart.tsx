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
 * 参考原型 brailleRender() 实现。
 */
export function GeekBrailleChart({ trend, cols = 88, rows = 10 }: GeekBrailleChartProps) {
  const result = useMemo(() => {
    if (trend.points.length === 0) return null;

    return brailleRender(
      [
        {
          values: trend.points.filter((p) => !p.gap).map((p) => p.input ?? 0),
          label: "input",
          primary: true,
        },
        {
          values: trend.points.filter((p) => !p.gap).map((p) => p.output ?? 0),
          label: "output",
        },
      ],
      { cols, rows, markCurrent: true },
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
        ▸ 末 4 列 = 当前未完成时段 · 按完成时刻计数 · 补报回填发生时段不算新消耗
      </div>
    </div>
  );
}
