import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useLocation, useSearchParams } from "react-router";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { OverviewPage } from "@/features/overview/OverviewPage";
import { RequestsPage } from "@/features/requests/RequestsPage";
import { ApiError } from "@/lib/api";
import type { Workspace } from "./api";
import {
  WorkspaceCreateDialog,
  type CreateSubmitScope,
} from "./WorkspaceCreateDialog";
import { WorkspaceSwitcher } from "./WorkspaceSwitcher";
import {
  isCurrentSessionUser,
  useWorkspaceList,
  workspaceDetailQuery,
} from "./queries";

function listErrorMessage(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.status === 403) return "无权限读取工作区列表。";
    if (error.status === 0) return "网络异常，请检查连接后重试。";
  }
  return "工作区列表加载失败，请稍后重试。";
}

/** 读取本容器写 URL 时带出的事件标记；其他来源的路由状态一律返回 null。 */
function readWriteToken(state: unknown): number | null {
  if (typeof state !== "object" || state === null) return null;
  const token: unknown = Reflect.get(state, "workspaceWrite");
  return typeof token === "number" ? token : null;
}

/**
 * 工作区容器：列表就绪前或为空时不挂载任何业务页面。
 * 当前空间只来自 URL 的 `workspace` 参数；参数缺失或不属于当前用户时回落到服务端首项。
 */
export function WorkspaceHome({
  userId,
  view = "overview",
}: {
  userId: string;
  view?: "overview" | "requests";
}) {
  const client = useQueryClient();
  const list = useWorkspaceList(userId);
  const [params, setParams] = useSearchParams();
  const routeState = useLocation().state;
  const [createOpen, setCreateOpen] = useState(false);
  const [pendingId, setPendingId] = useState<string | null>(null);
  const [switchError, setSwitchError] = useState<string | null>(null);
  /** 选择序号：每次最新选择自增，旧回调据此失效。 */
  const switchSeq = useRef(0);
  /** 本容器实例当前的身份；身份变化后旧选择不得再导航。 */
  const identity = useRef(userId);
  /** 挂载状态：卸载后的迟到回调只返回，不再设置状态或 URL。 */
  const mounted = useRef(true);
  /** 本容器发起、仍在等待的详情验证；卸载时只取消这一条。 */
  const activeDetail = useRef<{ userId: string; workspaceId: string } | null>(
    null,
  );
  /** 系统自己写 URL 时分配的事件标记；只有携带同一标记的路由事件才算系统写入。 */
  const writeSeq = useRef(0);
  const pendingWrite = useRef<number | null>(null);
  /** 上一次观察到的 URL 工作区选择；据此识别选择被外部改掉。 */
  const observedSelection = useRef<string | null | undefined>(undefined);

  /** 作废进行中的选择：递增序号、清掉 pending，旧回调此后一律提前返回。 */
  const resetSelection = useCallback(() => {
    switchSeq.current += 1;
    setPendingId(null);
  }, []);

  /**
   * 系统写 URL：给这次导航带一个一次性事件标记，观察者据此只认领自己写的那一次。
   * 不按目标值判断，所以外部把 URL 改到同一个值、或历史返回到旧位置都不会被误认。
   */
  const writeWorkspaceParam = useCallback(
    (workspaceId: string) => {
      const token = writeSeq.current + 1;
      writeSeq.current = token;
      pendingWrite.current = token;
      setParams(
        (prev) => {
          const next = new URLSearchParams(prev);
          next.set("workspace", workspaceId);
          return next;
        },
        { replace: true, state: { workspaceWrite: token } },
      );
    },
    [setParams],
  );

  const workspaces = useMemo(() => list.data ?? [], [list.data]);
  const urlWorkspaceId = params.get("workspace");
  const workspaceParamCount = params.getAll("workspace").length;
  const current = useMemo<Workspace | null>(() => {
    if (!list.isSuccess || workspaces.length === 0) return null;
    return (
      workspaces.find((w) => w.workspaceId === urlWorkspaceId) ??
      workspaces[0] ??
      null
    );
  }, [list.isSuccess, workspaces, urlWorkspaceId]);

  // 路由事件先结算，规范化再基于最新事件决定写什么：
  // 带本容器标记的那次写入算系统事件；其余 workspace 选择变化都算外部事件，作废旧选择。
  useEffect(() => {
    const selected = urlWorkspaceId;
    const written = readWriteToken(routeState);
    if (written !== null && written === pendingWrite.current) {
      // 认领后立刻清掉标记：同一位置再次出现（例如历史返回）不再算系统写入。
      pendingWrite.current = null;
      observedSelection.current = selected;
      return;
    }
    // 没有可认领的标记：旧标记随这次外部事件作废，避免晚到的事件冒领。
    pendingWrite.current = null;
    const previous = observedSelection.current;
    observedSelection.current = selected;
    if (previous === undefined || previous === selected) return;
    resetSelection();
    void client.cancelQueries({ queryKey: ["workspace", "detail", userId] });
  }, [client, resetSelection, routeState, urlWorkspaceId, userId]);

  // URL 无有效成员工作区时规范化到当前选择，其他查询参数原样保留。
  // 系统自己的规范化不算用户选择，所以不递增选择序号。
  useEffect(() => {
    if (!current) return;
    if (workspaceParamCount === 1 && urlWorkspaceId === current.workspaceId) {
      return;
    }
    writeWorkspaceParam(current.workspaceId);
  }, [current, writeWorkspaceParam, urlWorkspaceId, workspaceParamCount]);

  // 身份变化：旧身份进行中的选择作废，取消其详情验证，迟到的成功不得再改 URL。
  useEffect(() => {
    const previous = identity.current;
    if (previous === userId) return;
    identity.current = userId;
    resetSelection();
    setSwitchError(null);
    void client.cancelQueries({ queryKey: ["workspace", "detail", previous] });
  }, [client, resetSelection, userId]);

  // 卸载后旧回调同样作废：离开的容器不得再设置 URL 或状态。
  // 只取消本容器发起的那条详情验证，不动其他容器或新身份的任务。
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      switchSeq.current += 1;
      const detail = activeDetail.current;
      activeDetail.current = null;
      if (detail === null) return;
      void client.cancelQueries({
        queryKey: ["workspace", "detail", detail.userId, detail.workspaceId],
      });
    };
  }, [client]);

  function cancelWorkspaceBusiness(workspaceId: string): Promise<void> {
    return client.cancelQueries({
      predicate: (query) => query.meta?.workspaceId === workspaceId,
    });
  }

  /**
   * 每个 await 之后调用：仍持有当前 ticket 才能继续操作共享状态。
   * 更新选择已接管时纯返回；仍持有 ticket 但身份失效时只清掉自己的 pending。
   */
  function canContinue(ticket: number): boolean {
    if (switchSeq.current !== ticket) return false;
    if (isCurrentSessionUser(client, userId)) return true;
    resetSelection();
    return false;
  }

  /** 创建提交瞬间的归属；返回后凭它判断是否还能导航。 */
  const captureCreateScope = useCallback(
    (): CreateSubmitScope => ({ userId, ticket: switchSeq.current }),
    [userId],
  );

  /**
   * 创建完成时仍可导航：容器挂载、提交身份未变、选择序号未被接管。
   * 外部路由选择同样递增序号，所以这里只认事件历史，不用 URL 是否相等替代。
   */
  function canNavigateAfterCreate(scope: CreateSubmitScope): boolean {
    if (!mounted.current) return false;
    if (scope.userId !== userId) return false;
    return canContinue(scope.ticket);
  }

  async function selectWorkspace(target: Workspace) {
    if (!current) return;
    if (pendingId === target.workspaceId) return;
    const selectingCurrent = target.workspaceId === current.workspaceId;
    // 已在该空间且没有待处理的切换或错误时，没有可作废的选择。
    if (selectingCurrent && pendingId === null && switchError === null) return;

    // 选中当前空间也是一次最新选择：旧回调立即失效，pending 与错误一并清掉。
    const ticket = switchSeq.current + 1;
    switchSeq.current = ticket;
    setSwitchError(null);
    setPendingId(selectingCurrent ? null : target.workspaceId);
    // 取消上一个详情验证；新选择到来时旧请求不会再发或再写 URL。
    await client.cancelQueries({
      queryKey: ["workspace", "detail", userId],
    });
    if (!canContinue(ticket)) return;
    if (selectingCurrent) return;
    const detail = { userId, workspaceId: target.workspaceId };
    activeDetail.current = detail;
    try {
      await client.fetchQuery(workspaceDetailQuery(userId, target.workspaceId));
      if (!canContinue(ticket)) return;
      await cancelWorkspaceBusiness(current.workspaceId);
      if (!canContinue(ticket)) return;
      writeWorkspaceParam(target.workspaceId);
      setPendingId(null);
    } catch (error) {
      if (!canContinue(ticket)) return;
      setPendingId(null);
      setSwitchError(
        error instanceof ApiError && error.status === 403
          ? `无法切换到「${target.name}」：没有访问权限。`
          : "切换失败，请重试。",
      );
    } finally {
      // 只清自己登记的那条，避免覆盖更新选择刚登记的详情验证。
      if (activeDetail.current === detail) activeDetail.current = null;
    }
  }

  async function handleCreated(created: Workspace, scope: CreateSubmitScope) {
    // 创建已成功、列表已重查；此后只在容器、身份和选择序号都仍有效时导航。
    if (!canNavigateAfterCreate(scope)) return;
    await client.cancelQueries({ queryKey: ["workspace", "detail", userId] });
    if (!canNavigateAfterCreate(scope)) return;
    if (current) await cancelWorkspaceBusiness(current.workspaceId);
    if (!canNavigateAfterCreate(scope)) return;
    setPendingId(null);
    setSwitchError(null);
    setCreateOpen(false);
    writeWorkspaceParam(created.workspaceId);
  }

  if (list.isPending) {
    return (
      <p role="status" className="text-muted-foreground text-sm">
        正在加载工作区…
      </p>
    );
  }

  if (list.isError) {
    return (
      <div className="flex flex-col gap-2">
        <Alert variant="destructive">
          <AlertDescription>{listErrorMessage(list.error)}</AlertDescription>
        </Alert>
        <Button
          variant="outline"
          className="self-start"
          onClick={() => void list.refetch()}
        >
          重试
        </Button>
      </div>
    );
  }

  if (!current) {
    // 零空间是正常状态：强制创建，创建成功前不挂载 Overview。
    return (
      <WorkspaceCreateDialog
        userId={userId}
        required
        onClose={() => undefined}
        captureScope={captureCreateScope}
        onCreated={handleCreated}
      />
    );
  }

  return (
    <div className="flex flex-col gap-4">
      <WorkspaceSwitcher
        workspaces={workspaces}
        current={current}
        pendingId={pendingId}
        error={switchError}
        onCreate={() => setCreateOpen(true)}
        onSelect={(workspace) => void selectWorkspace(workspace)}
      />
      {view === "requests" ? (
        <RequestsPage
          key={current.workspaceId}
          userId={userId}
          workspaceId={current.workspaceId}
        />
      ) : (
        <OverviewPage
          key={current.workspaceId}
          userId={userId}
          workspaceId={current.workspaceId}
        />
      )}
      {createOpen ? (
        <WorkspaceCreateDialog
          userId={userId}
          required={false}
          onClose={() => setCreateOpen(false)}
          captureScope={captureCreateScope}
          onCreated={handleCreated}
        />
      ) : null}
    </div>
  );
}
