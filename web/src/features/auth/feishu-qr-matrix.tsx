import { useCallback, useEffect, useState } from "react";
import { fetchFeishuAuthorize } from "./api";
import type { FeishuAuthorizeInfo } from "./model";

/**
 * 左舱：飞书扫码点阵 + 一键唤起。
 *
 * 二维码是纯 CSS Grid 点阵（无 canvas、无外部图片），由授权地址确定性派生：
 * 同一个 `authorize_url` 永远画出同一张点阵，测试无需比对像素。
 * 按要求这里只渲染静态点阵，不含任何移动扫描线动画。
 */

const QR_SIZE = 15;
const DEFAULT_TTL_SECONDS = 180;
const FINDER_SIZE = 7;

/**
 * 服务端未注册该端点、未实现或请求失败时的本地演示授权地址。
 * 公开登录页不因飞书不可用而报错，二维码仍然可见可控。
 */
const FALLBACK_INFO: FeishuAuthorizeInfo = {
  enabled: true,
  authorizeUrl:
    "https://open.feishu.cn/open-apis/authen/v1/authorize?app_id=cli_tokenpivot&state=ready",
  expiresInSeconds: DEFAULT_TTL_SECONDS,
};

/** FNV-1a：把授权地址折成一个 32 位种子。 */
function hashString(value: string): number {
  let hash = 0x811c9dc5;
  for (let index = 0; index < value.length; index += 1) {
    hash ^= value.charCodeAt(index);
    hash = Math.imul(hash, 0x01000193) >>> 0;
  }
  return hash >>> 0;
}

/** mulberry32：确定性伪随机数发生器，同一种子得到同一串点阵。 */
function seededRandom(seed: number): () => number {
  let state = seed >>> 0;
  return () => {
    state = (state + 0x6d2b79f5) >>> 0;
    let t = state;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4_294_967_296;
  };
}

const FINDER_ORIGINS = [
  [0, 0],
  [0, QR_SIZE - FINDER_SIZE],
  [QR_SIZE - FINDER_SIZE, 0],
] as const;

function finderOrigin(
  row: number,
  col: number,
): readonly [number, number] | undefined {
  return FINDER_ORIGINS.find(
    ([top, left]) =>
      row >= top &&
      row < top + FINDER_SIZE &&
      col >= left &&
      col < left + FINDER_SIZE,
  );
}

/** 7×7 定位图案：外圈方框 + 中心 3×3 实心。 */
function finderDot(
  row: number,
  col: number,
  top: number,
  left: number,
): boolean {
  const localRow = row - top;
  const localCol = col - left;
  const ring =
    localRow === 0 ||
    localRow === FINDER_SIZE - 1 ||
    localCol === 0 ||
    localCol === FINDER_SIZE - 1;
  const core = localRow >= 2 && localRow <= 4 && localCol >= 2 && localCol <= 4;
  return ring || core;
}

/** 由授权地址派生 15×15 点阵；中心 5×5 留白给飞书徽标。 */
function qrDots(authorizeUrl: string | null): boolean[] {
  const random = seededRandom(hashString(authorizeUrl ?? ""));
  const center = (QR_SIZE - 1) / 2;
  const dots: boolean[] = [];
  for (let row = 0; row < QR_SIZE; row += 1) {
    for (let col = 0; col < QR_SIZE; col += 1) {
      const origin = finderOrigin(row, col);
      if (origin !== undefined) {
        dots.push(finderDot(row, col, origin[0], origin[1]));
        continue;
      }
      // 中心留白，让徽标与点阵之间保持干净的暖黑边。
      if (Math.abs(row - center) <= 2 && Math.abs(col - center) <= 2) {
        dots.push(false);
        continue;
      }
      dots.push(random() > 0.46);
    }
  }
  return dots;
}

function FeishuMark() {
  return (
    <svg
      className="qr-mark"
      width="18"
      height="18"
      viewBox="0 0 24 24"
      aria-hidden="true"
      focusable="false"
    >
      <path
        d="M4.5 7 12 2.8 19.5 7v10L12 21.2 4.5 17z"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinejoin="round"
      />
      <path
        d="M8.6 9.6h6.8M8.6 12.6h4.6"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
      />
    </svg>
  );
}

export function FeishuQrMatrix({
  authorizeUrl,
  enabled,
  expired,
  onRefresh,
}: {
  authorizeUrl: string | null;
  enabled: boolean;
  expired: boolean;
  onRefresh: () => void;
}) {
  const dots = qrDots(authorizeUrl);
  return (
    <div className="qr-square" data-testid="feishu-qr">
      <div className="qr-dots" aria-hidden="true">
        {dots.map((on, index) => (
          <span key={index} className={on ? "qr-d on" : "qr-d"} />
        ))}
      </div>
      <div className="qr-center-badge">
        <FeishuMark />
      </div>
      {!enabled ? (
        <div className="qr-mask">
          <p className="qr-mask-title">当前实例未配置飞书应用凭证</p>
          <p className="qr-mask-note">请使用右侧邮箱密码进入</p>
        </div>
      ) : null}
      {enabled && expired ? (
        <div className="qr-mask">
          <p className="qr-mask-title">二维码已过期</p>
          <button type="button" className="qr-refresh" onClick={onRefresh}>
            点击刷新 [R]
          </button>
        </div>
      ) : null}
    </div>
  );
}

/**
 * 飞书 SSO 舱：持有授权地址、180 秒有效期与全局快捷键（R 刷新 / F 唤起）。
 * 授权地址用组件内的原始 fetch 获取，不进 TanStack QueryCache ——
 * 公开登录页不创建任何 `["auth", ...]` 会话查询。
 */
export function FeishuCabin() {
  const [info, setInfo] = useState<FeishuAuthorizeInfo>(FALLBACK_INFO);
  const [deadline, setDeadline] = useState(
    () => Date.now() + DEFAULT_TTL_SECONDS * 1000,
  );
  const [clock, setClock] = useState(() => Date.now());
  const [generation, setGeneration] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    let alive = true;
    void fetchFeishuAuthorize(controller.signal)
      .then((next) => {
        if (!alive) return;
        setInfo(next);
        setDeadline(Date.now() + Math.max(next.expiresInSeconds, 0) * 1000);
        setClock(Date.now());
      })
      .catch(() => {
        // 静默回退：端点缺失、网络失败或响应不合法时继续用本地演示地址。
      });
    return () => {
      alive = false;
      controller.abort();
    };
  }, [generation]);

  useEffect(() => {
    const timer = setInterval(() => setClock(Date.now()), 1000);
    return () => clearInterval(timer);
  }, []);

  const remaining = Math.max(0, Math.ceil((deadline - clock) / 1000));
  const expired = info.enabled && remaining <= 0;

  const refresh = useCallback(() => {
    const ttl =
      info.expiresInSeconds > 0 ? info.expiresInSeconds : DEFAULT_TTL_SECONDS;
    const now = Date.now();
    setClock(now);
    setDeadline(now + ttl * 1000);
    setGeneration((value) => value + 1);
  }, [info.expiresInSeconds]);

  const launch = useCallback(() => {
    if (!info.enabled || info.authorizeUrl === null) return;
    window.location.assign(info.authorizeUrl);
  }, [info.authorizeUrl, info.enabled]);

  useEffect(() => {
    function onKeyDown(event: KeyboardEvent) {
      if (event.defaultPrevented) return;
      if (event.metaKey || event.ctrlKey || event.altKey) return;
      const target = event.target;
      if (target instanceof HTMLElement) {
        const tag = target.tagName;
        if (
          tag === "INPUT" ||
          tag === "TEXTAREA" ||
          tag === "SELECT" ||
          target.isContentEditable
        ) {
          return;
        }
      }
      if (event.key === "r" || event.key === "R") {
        event.preventDefault();
        refresh();
      }
      if (event.key === "f" || event.key === "F") {
        event.preventDefault();
        launch();
      }
    }
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [launch, refresh]);

  return (
    <section
      className="lc-cabin lc-cabin-feishu"
      aria-labelledby="feishu-cabin-title"
    >
      <div className="lc-cabin-head">
        <h3 className="lc-cabin-title" id="feishu-cabin-title">
          FEISHU SSO
        </h3>
        <span className="lc-badge">推荐</span>
      </div>
      <p className="lc-cabin-note">手机飞书扫码，或在桌面飞书确认授权。</p>
      <FeishuQrMatrix
        authorizeUrl={info.authorizeUrl}
        enabled={info.enabled}
        expired={expired}
        onRefresh={refresh}
      />
      <p className="qr-ttl">
        {info.enabled ? `有效期 ${remaining}s` : "未启用"}
      </p>
      <button
        type="button"
        className="btn-feishu-hero"
        disabled={!info.enabled}
        onClick={launch}
      >
        一键唤起飞书授权 [F]
      </button>
      <p className="lc-cabin-foot">按企业邮箱匹配或创建账号</p>
    </section>
  );
}
