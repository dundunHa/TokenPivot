import { useState } from "react";
import { brailleRender } from "@/features/overview/geek/braille";

/**
 * 未登录态左栏的确定性遥测快照。
 *
 * 全部数据写死在本文件里：未登录页不查询任何统计接口，也不读取本地缓存，
 * 渲染结果在不同会话、不同机器上完全一致，因此可以直接被快照测试断言。
 */

/** 最近 34 分钟的 input 速率（单位：千 tok/min）。 */
const IN_SERIES: readonly number[] = [
  44, 49, 46, 55, 68, 82, 94, 88, 76, 68, 72, 85, 106, 128, 134, 118, 96, 84,
  80, 92, 115, 144, 166, 154, 132, 110, 94, 90, 108, 132, 156, 174, 186, 170,
];

/** 最近 34 分钟的 output 速率（单位：千 tok/min）。 */
const OUT_SERIES: readonly number[] = [
  14, 16, 15, 19, 24, 30, 36, 33, 28, 24, 26, 31, 39, 48, 51, 44, 35, 30, 28,
  33, 42, 54, 64, 59, 49, 40, 34, 32, 39, 49, 58, 66, 72, 64,
];

/** 4×2 Braille 点阵曲线：34 列 × 5 行，Y 轴固定 0–260k 便于逐次对比。 */
const CHART = brailleRender(
  [
    { values: [...IN_SERIES], label: "input", primary: true },
    { values: [...OUT_SERIES], label: "output" },
  ],
  { cols: IN_SERIES.length, rows: 5, minY: 0, maxY: 260 },
);

const Y_AXIS = ["260k", "195k", "130k", " 65k", "  0k"] as const;

const METRICS = [
  { label: "INPUT TOKENS", value: "52.4M", note: "cache 68.2%" },
  { label: "OUTPUT TOKENS", value: "14.8M", note: "22.0% share" },
  { label: "REQUESTS", value: "4,812", note: "✓ 99.4% known" },
  { label: "60S RATE", value: "2,482/s", note: "peak @ 14:20" },
] as const;

type Dimension = "model" | "provider" | "project";

const DIMENSIONS = [
  { key: "model", label: "按模型" },
  { key: "provider", label: "按供应商" },
  { key: "project", label: "按项目" },
] as const;

interface RankRow {
  name: string;
  value: string;
  share: string;
  /** 18 格字符进度条里实心的格数。 */
  fill: number;
}

const BAR_WIDTH = 18;

const RANKINGS: Record<Dimension, readonly RankRow[]> = {
  model: [
    { name: "claude-sonnet-4-5", value: "31.4M", share: "46.7%", fill: 15 },
    { name: "deepseek-v3.2", value: "19.8M", share: "29.5%", fill: 10 },
    { name: "gpt-5.4", value: "11.2M", share: "16.7%", fill: 6 },
    { name: "其他 (3 models)", value: "4.8M", share: "7.1%", fill: 3 },
  ],
  provider: [
    { name: "anthropic", value: "31.4M", share: "46.7%", fill: 15 },
    { name: "deepseek", value: "22.6M", share: "33.6%", fill: 11 },
    { name: "openai", value: "13.2M", share: "19.7%", fill: 7 },
  ],
  project: [
    { name: "tokenpivot-core", value: "28.9M", share: "43.0%", fill: 14 },
    { name: "dsh-harness", value: "24.1M", share: "35.9%", fill: 12 },
    { name: "pi-collector", value: "14.2M", share: "21.1%", fill: 7 },
  ],
};

/** 千 tok/min → 带千分位的整数读数。 */
function formatTokenRate(kTokPerMin: number): string {
  return (kTokPerMin * 1000).toLocaleString("en-US");
}

export function AuthDashboardSnapshot() {
  const [dimension, setDimension] = useState<Dimension>("model");
  const [hovered, setHovered] = useState<number | null>(null);

  const rows = RANKINGS[dimension];
  const latest = IN_SERIES.length - 1;
  const active = hovered ?? latest;
  const readoutLabel = hovered === null ? "LATEST" : `T-${latest - hovered}m`;
  const readout = `${readoutLabel}: ${formatTokenRate(
    (IN_SERIES[active] ?? 0) + (OUT_SERIES[active] ?? 0),
  )} tok/min (in ${IN_SERIES[active] ?? 0}k · out ${OUT_SERIES[active] ?? 0}k)`;

  return (
    <section className="dash-snapshot" aria-label="TokenPivot 遥测快照">
      <div className="ds-meta">
        <p className="ds-meta-title">
          LIVE SNAPSHOT · TODAY (00:00–15:45 UTC+8)
        </p>
        <div className="ds-window">
          <span className="ds-window-pill on">1m</span>
          <span className="ds-window-pill">1h</span>
          <span className="ds-window-pill">1d</span>
        </div>
      </div>

      <div className="ds-sections-grid">
        <div className="ds-section">
          <h2 className="ds-sec-title">
            00 TOTAL TOKENS
            <span className="ds-sec-note">· 今日累计消耗 (input + output)</span>
          </h2>
          <div className="ds-hero-summary">
            <p className="ds-hero">
              <span className="ds-hero-num">67,248,910</span>
              <span className="ds-hero-unit">tokens</span>
            </p>
            <p className="ds-rolling">ROLLING 60S: 148,920 tok/min</p>
          </div>
          <div className="ds-metrics-grid">
            {METRICS.map((metric) => (
              <div className="ds-metric" key={metric.label}>
                <p className="ds-metric-label">{metric.label}</p>
                <p className="ds-metric-value">{metric.value}</p>
                <p className="ds-metric-note">{metric.note}</p>
              </div>
            ))}
          </div>
        </div>

        <div className="ds-section">
          <h2 className="ds-sec-title">
            01 MINUTE TOKEN VELOCITY
            <span className="ds-sec-note">· Braille 4×2 速率曲线</span>
          </h2>
          <div className="ds-chart" onMouseLeave={() => setHovered(null)}>
            <div className="ds-chart-y" aria-hidden="true">
              {Y_AXIS.map((label) => (
                <span key={label}>{label}</span>
              ))}
            </div>
            <div className="ds-chart-plot">
              {CHART.lines.map((line, index) => (
                <div className="ds-chart-line" key={index}>
                  {line}
                </div>
              ))}
              <div className="ds-chart-cols" aria-hidden="true">
                {IN_SERIES.map((_, index) => (
                  <span
                    key={index}
                    className={index === hovered ? "ds-col on" : "ds-col"}
                    onMouseEnter={() => setHovered(index)}
                  />
                ))}
              </div>
            </div>
          </div>
          <p className="ds-readout" data-testid="snapshot-readout">
            {readout}
          </p>
          <p className="ds-legend">▪ input · ▫ output · 每列为一分钟</p>
        </div>

        <div className="ds-section">
          <h2 className="ds-sec-title">
            02 TOP CONSUMPTION RANKING
            <span className="ds-sec-note">· 来源消耗排行</span>
          </h2>
          <div className="ds-rank-tabs" role="group" aria-label="排行维度">
            {DIMENSIONS.map((item) => (
              <button
                key={item.key}
                type="button"
                className={
                  item.key === dimension ? "ds-rank-tab on" : "ds-rank-tab"
                }
                aria-pressed={item.key === dimension}
                onClick={() => setDimension(item.key)}
              >
                {item.label}
              </button>
            ))}
          </div>
          <ul className="ds-rank-list">
            {rows.map((row) => (
              <li className="ds-rank-row" key={row.name}>
                <span className="ds-rank-name">{row.name}</span>
                <span className="ds-rank-bar" aria-hidden="true">
                  {"█".repeat(row.fill)}
                  {"░".repeat(BAR_WIDTH - row.fill)}
                </span>
                <span className="ds-rank-value">{row.value}</span>
                <span className="ds-rank-share">{row.share}</span>
              </li>
            ))}
          </ul>
        </div>
      </div>
    </section>
  );
}
