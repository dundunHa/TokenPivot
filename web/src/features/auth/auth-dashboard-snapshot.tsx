import { useState } from "react";

/** 公开登录页的确定性演示数据，不创建统计或会话请求。 */
const IN_SERIES: readonly number[] = [
  44, 49, 46, 55, 68, 82, 94, 88, 76, 68, 72, 85, 106, 128, 134, 118, 96, 84,
  80, 92, 115, 144, 166, 154, 132, 110, 94, 90, 108, 132, 156, 174, 186, 170,
];
const OUT_SERIES: readonly number[] = [
  14, 16, 15, 19, 24, 30, 36, 33, 28, 24, 26, 31, 39, 48, 51, 44, 35, 30, 28,
  33, 42, 54, 64, 59, 49, 40, 34, 32, 39, 49, 58, 66, 72, 64,
];
const TOTAL_SERIES = IN_SERIES.map(
  (value, index) => value + (OUT_SERIES[index] ?? 0),
);

const METRICS = [
  { label: "输入 Token", value: "52.4M", note: "77.9% 总量" },
  { label: "输出 Token", value: "14.8M", note: "22.1% 总量" },
  { label: "请求数", value: "4,812", note: "99.4% 用量已知" },
  { label: "当前速率", value: "234K", note: "tok / min" },
] as const;

type TrendDimension = "model" | "provider";
type Tone = "amber" | "green" | "copper";

interface TrendSeries {
  name: string;
  tone: Tone;
  values: readonly number[];
}

const DIMENSIONS = [
  { key: "model", label: "按模型" },
  { key: "provider", label: "按提供商" },
] as const;

function makeSeries(weight: number, phase: number): readonly number[] {
  return TOTAL_SERIES.map((value, index) =>
    Math.max(
      0,
      Math.round(value * weight + Math.sin((index + phase) * 0.65) * 6),
    ),
  );
}

const TRENDS: Record<TrendDimension, readonly TrendSeries[]> = {
  model: [
    { name: "Claude Sonnet 4.5", tone: "amber", values: makeSeries(0.46, 0) },
    { name: "DeepSeek v3.2", tone: "green", values: makeSeries(0.29, 4) },
    { name: "GPT-5.4", tone: "copper", values: makeSeries(0.16, 8) },
  ],
  provider: [
    { name: "Anthropic", tone: "amber", values: makeSeries(0.47, 2) },
    { name: "DeepSeek", tone: "green", values: makeSeries(0.33, 6) },
    { name: "OpenAI", tone: "copper", values: makeSeries(0.19, 10) },
  ],
};

const WIDTH = 720;
const HEIGHT = 260;
const LEFT = 48;
const RIGHT = 16;
const TOP = 18;
const BOTTOM = 34;
const PLOT_WIDTH = WIDTH - LEFT - RIGHT;
const PLOT_HEIGHT = HEIGHT - TOP - BOTTOM;
const BASE_Y = HEIGHT - BOTTOM;
const X_LABELS = [
  { index: 0, label: "15:12" },
  { index: 11, label: "15:23" },
  { index: 22, label: "15:34" },
  { index: 33, label: "15:45" },
] as const;

interface Point {
  x: number;
  y: number;
}

function makePoints(values: readonly number[], maxY: number): Point[] {
  return values.map((value, index) => ({
    x: LEFT + (index / (values.length - 1)) * PLOT_WIDTH,
    y: TOP + (1 - value / maxY) * PLOT_HEIGHT,
  }));
}

function smoothPath(points: readonly Point[]): string {
  const first = points[0];
  if (!first) return "";
  let path = `M ${first.x} ${first.y}`;
  for (let index = 1; index < points.length; index += 1) {
    const previous = points[index - 1];
    const point = points[index];
    if (!previous || !point) continue;
    const middle = (previous.x + point.x) / 2;
    path += ` C ${middle} ${previous.y}, ${middle} ${point.y}, ${point.x} ${point.y}`;
  }
  return path;
}

function areaPath(points: readonly Point[]): string {
  const first = points[0];
  const last = points[points.length - 1];
  if (!first || !last) return "";
  return `${smoothPath(points)} L ${last.x} ${BASE_Y} L ${first.x} ${BASE_Y} Z`;
}

function formatK(value: number): string {
  return `${Math.round(value)}K`;
}

export function AuthDashboardSnapshot() {
  const [dimension, setDimension] = useState<TrendDimension>("model");
  const [hovered, setHovered] = useState<number | null>(null);
  const series = TRENDS[dimension];
  const latest = TOTAL_SERIES.length - 1;
  const active = hovered ?? latest;
  const maxY =
    Math.ceil(Math.max(...series.flatMap((item) => [...item.values])) / 40) *
    40;
  const yTicks = [1, 0.75, 0.5, 0.25, 0];
  const activeTime = hovered === null ? "最新" : `T-${latest - active}m`;

  return (
    <section className="dash-snapshot" aria-label="TokenPivot Token 用量仪表盘">
      <div className="usage-head">
        <div>
          <p className="usage-eyebrow">USAGE ANALYTICS</p>
          <h2 className="usage-title">今日 Token 用量</h2>
        </div>
        <span className="usage-window">近 34 分钟</span>
      </div>

      <div className="usage-total">
        <div className="usage-total-line">
          <strong>67.25</strong>
          <span>M Tokens</span>
        </div>
        <p className="usage-delta">
          ↗ 12.8% <span>较昨日</span>
        </p>
      </div>

      <div className="usage-metrics" aria-label="关键指标">
        {METRICS.map((metric) => (
          <div className="usage-metric" key={metric.label}>
            <p>{metric.label}</p>
            <strong>{metric.value}</strong>
            <span>{metric.note}</span>
          </div>
        ))}
      </div>

      <div className="usage-trend">
        <div className="usage-trend-head">
          <div>
            <h3>Token 速率</h3>
            <p>
              {activeTime} · {formatK(TOTAL_SERIES[active] ?? 0)} tok/min
            </p>
          </div>
          <div className="usage-tabs" role="group" aria-label="趋势维度">
            {DIMENSIONS.map((item) => (
              <button
                key={item.key}
                type="button"
                className={item.key === dimension ? "on" : undefined}
                aria-pressed={item.key === dimension}
                onClick={() => {
                  setDimension(item.key);
                  setHovered(null);
                }}
              >
                {item.label}
              </button>
            ))}
          </div>
        </div>

        <div className="usage-chart" data-testid="usage-trend-chart">
          <svg
            viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
            role="img"
            aria-label={`${dimension === "model" ? "按模型" : "按提供商"} Token 速率趋势`}
            onMouseLeave={() => setHovered(null)}
          >
            <defs>
              {series.map((item) => (
                <linearGradient
                  key={item.tone}
                  id={`usage-${item.tone}`}
                  x1="0"
                  y1="0"
                  x2="0"
                  y2="1"
                >
                  <stop className={`usage-stop-${item.tone}`} offset="0%" />
                  <stop className="usage-stop-clear" offset="100%" />
                </linearGradient>
              ))}
            </defs>

            {yTicks.map((tick) => {
              const y = TOP + tick * PLOT_HEIGHT;
              return (
                <g key={tick} className="usage-grid">
                  <line x1={LEFT} x2={WIDTH - RIGHT} y1={y} y2={y} />
                  <text x={LEFT - 10} y={y + 3} textAnchor="end">
                    {formatK(maxY * (1 - tick))}
                  </text>
                </g>
              );
            })}
            {X_LABELS.map((item) => (
              <text
                key={item.label}
                className="usage-x-label"
                x={LEFT + (item.index / latest) * PLOT_WIDTH}
                y={HEIGHT - 10}
                textAnchor={
                  item.index === 0
                    ? "start"
                    : item.index === latest
                      ? "end"
                      : "middle"
                }
              >
                {item.label}
              </text>
            ))}

            {series.map((item) => {
              const points = makePoints(item.values, maxY);
              return (
                <g key={`${dimension}-${item.name}`}>
                  <path
                    className="usage-area"
                    d={areaPath(points)}
                    fill={`url(#usage-${item.tone})`}
                  />
                  <path
                    className={`usage-line usage-${item.tone}`}
                    d={smoothPath(points)}
                    pathLength={1}
                  />
                  <circle
                    className={`usage-point usage-${item.tone}`}
                    cx={points[active]?.x}
                    cy={points[active]?.y}
                    r={hovered === null ? 3.5 : 4.5}
                  />
                </g>
              );
            })}

            {hovered !== null ? (
              <line
                className="usage-cursor"
                x1={LEFT + (active / latest) * PLOT_WIDTH}
                x2={LEFT + (active / latest) * PLOT_WIDTH}
                y1={TOP}
                y2={BASE_Y}
              />
            ) : null}
            {TOTAL_SERIES.map((_, index) => (
              <rect
                key={index}
                x={
                  LEFT + (index / latest) * PLOT_WIDTH - PLOT_WIDTH / latest / 2
                }
                y={TOP}
                width={PLOT_WIDTH / latest}
                height={PLOT_HEIGHT}
                fill="transparent"
                onMouseEnter={() => setHovered(index)}
              />
            ))}
          </svg>
        </div>

        <div className="usage-legend" aria-label="趋势图例">
          {series.map((item) => (
            <div key={item.name} className="usage-legend-item">
              <span className={`usage-dot usage-${item.tone}`} />
              <span>{item.name}</span>
              <strong>{formatK(item.values[active] ?? 0)}</strong>
            </div>
          ))}
        </div>
        <p className="sr-only" data-testid="snapshot-readout">
          {activeTime}: {formatK(TOTAL_SERIES[active] ?? 0)} tok/min
        </p>
      </div>
    </section>
  );
}
