/**
 * braille.ts — 4×2 点阵真曲线光栅化
 *
 * 把数值序列渲染为 Unicode Braille 字符画。
 * 每个 braille 字符是 2 列 × 4 行点阵，8 个点位对应 8 个 bit。
 *
 * 参考：docs/prototypes/dashboard-j.html brailleRender()
 */

/** Braille 点位 bit 映射：[col][row] → bit mask */
const BRAILLE_BITS: number[][] = [
  [0x01, 0x08], // col 0: dot1(0,0), dot4(0,1)
  [0x02, 0x10], // col 1: dot2(1,0), dot5(1,1)
  [0x04, 0x20], // col 2: dot3(0,2), dot6(1,2)
  [0x40, 0x80], // col 3: dot7(0,3), dot8(1,3)
];

const BRAILLE_BASE = 0x2800;

export interface BrailleSeries {
  /** 数值序列 */
  values: number[];
  /** 显示标签 */
  label: string;
  /** 是否主序列（加粗渲染） */
  primary?: boolean;
}

export interface BrailleOptions {
  /** 字符列数（每字符 2 像素宽） */
  cols: number;
  /** 字符行数（每字符 4 像素高） */
  rows: number;
  /** Y 轴最小值（默认自动） */
  minY?: number;
  /** Y 轴最大值（默认自动） */
  maxY?: number;
  /** 是否标记最后一列为当前未完成时段 */
  markCurrent?: boolean;
}

export interface BrailleResult {
  /** 渲染后的字符行（每行一个字符串） */
  lines: string[];
  /** X 轴标签行 */
  axisLine: string;
  /** 图例行 */
  legendLine: string;
  /** Y 轴范围 */
  yRange: { min: number; max: number };
}

/**
 * 将多条数值序列渲染为 braille 字符画。
 */
export function brailleRender(
  series: BrailleSeries[],
  opts: BrailleOptions,
): BrailleResult {
  const { cols, rows, markCurrent = false } = opts;
  const pixelW = cols * 2;
  const pixelH = rows * 4;

  // 像素缓冲区：每个像素存 series index（-1 = 空）
  const buf: Int8Array = new Int8Array(pixelW * pixelH).fill(-1);

  // 计算 Y 范围
  let minY = opts.minY ?? Infinity;
  let maxY = opts.maxY ?? -Infinity;
  for (const s of series) {
    for (const v of s.values) {
      if (v < minY) minY = v;
      if (v > maxY) maxY = v;
    }
  }
  if (minY === maxY) {
    minY = Math.min(0, minY);
    maxY = maxY + 1;
  }
  const ySpan = maxY - minY;

  // 绘制每条序列
  for (let si = 0; si < series.length; si++) {
    const s = series[si];
    const n = s.values.length;
    if (n < 2) continue;

    for (let i = 0; i < n - 1; i++) {
      const x0 = Math.round((i / (n - 1)) * (pixelW - 1));
      const x1 = Math.round(((i + 1) / (n - 1)) * (pixelW - 1));
      const y0 = Math.round((1 - (s.values[i] - minY) / ySpan) * (pixelH - 1));
      const y1 = Math.round(
        (1 - (s.values[i + 1] - minY) / ySpan) * (pixelH - 1),
      );

      // Bresenham 画线
      const dx = Math.abs(x1 - x0);
      const dy = Math.abs(y1 - y0);
      const sx = x0 < x1 ? 1 : -1;
      const sy = y0 < y1 ? 1 : -1;
      let err = dx - dy;
      let cx = x0;
      let cy = y0;

      for (;;) {
        // 主序列加粗：plot 2×2
        if (s.primary) {
          for (let ox = 0; ox <= 1; ox++) {
            for (let oy = 0; oy <= 1; oy++) {
              const px = cx + ox;
              const py = cy + oy;
              if (px >= 0 && px < pixelW && py >= 0 && py < pixelH) {
                buf[py * pixelW + px] = si;
              }
            }
          }
        } else {
          if (cx >= 0 && cx < pixelW && cy >= 0 && cy < pixelH) {
            buf[cy * pixelW + cx] = si;
          }
        }

        if (cx === x1 && cy === y1) break;
        const e2 = 2 * err;
        if (e2 > -dy) {
          err -= dy;
          cx += sx;
        }
        if (e2 < dx) {
          err += dx;
          cy += sy;
        }
      }
    }
  }

  // 像素 → braille 字符
  const lines: string[] = [];
  for (let row = 0; row < rows; row++) {
    let line = "";
    for (let col = 0; col < cols; col++) {
      let bits = 0;
      for (let dr = 0; dr < 4; dr++) {
        for (let dc = 0; dc < 2; dc++) {
          const px = col * 2 + dc;
          const py = row * 4 + dr;
          const si = buf[py * pixelW + px];
          if (si >= 0) {
            bits |= BRAILLE_BITS[dr][dc];
          }
        }
      }
      line += String.fromCharCode(BRAILLE_BASE + bits);
    }
    lines.push(line);
  }

  // X 轴标签
  const axisChars = new Array(cols).fill(" ");
  const axisLabels = ["0", "¼", "½", "¾", "1"];
  for (let i = 0; i < axisLabels.length; i++) {
    const pos = Math.round((i / (axisLabels.length - 1)) * (cols - 1));
    const label = axisLabels[i];
    for (let j = 0; j < label.length && pos + j < cols; j++) {
      axisChars[pos + j] = label[j];
    }
  }
  const axisLine = axisChars.join("");

  // 图例
  const legendLine = series
    .map((s) => `${s.primary ? "▪" : "▫"} ${s.label}`)
    .join("  ");

  // 当前未完成时段标记
  if (markCurrent && lines.length > 0) {
    // 末 4 列用 dim 标记（在组件层处理）
  }

  return { lines, axisLine, legendLine, yRange: { min: minY, max: maxY } };
}

/**
 * 将数值格式化为紧凑形式（K/M/B）。
 */
export function fmtCompact(n: number): string {
  if (n >= 1e9) return (n / 1e9).toFixed(2) + "B";
  if (n >= 1e6) return (n / 1e6).toFixed(2) + "M";
  if (n >= 1e3) return (n / 1e3).toFixed(1) + "K";
  return String(Math.round(n));
}
