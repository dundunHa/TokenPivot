import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useLocation, useSearchParams } from "react-router";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { AppTopbar } from "@/components/AppTopbar";
import { OverviewPage } from "@/features/overview/OverviewPage";
import { RequestsPage } from "@/features/requests/RequestsPage";
import { ApiError } from "@/lib/api";
import type { Workspace } from "./api";
import {
  WorkspaceCreateDialog,
  type CreateSubmitScope,
} from "./WorkspaceCreateDialog";
import { WorkspaceSwitcher, WorkspaceSwitchError } from "./WorkspaceSwitcher";
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

export interface WorkspaceHeaderProps {
  email: string;
  logoutPending: boolean;
  logoutError: string | null;
  onLogout: () => void;
}

/**
 * 工作区容器：列表就绪前或为空时不挂载任何业务页面。
 * 当前空间只来自 URL 的 `workspace` 参数；参数缺失或不属于当前用户时回落到服务端首项。
 */
export function WorkspaceHome({
  userId,
  view = "overview",
  header,
}: {
  userId: string;
  view?: "overview" | "requests";
  header?: WorkspaceHeaderProps;
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

  useEffect(() => {
    const selected = urlWorkspaceId;
    const written = readWriteToken(routeState);
    if (written !== null && written === pendingWrite.current) {
      pendingWrite.current = null;
      observedSelection.current = selected;
      return;
    }
    pendingWrite.current = null;
    const previous = observedSelection.current;
    observedSelection.current = selected;
    if (previous === undefined || previous === selected) return;
    resetSelection();
    void client.cancelQueries({ queryKey: ["workspace", "detail", userId] });
  }, [client, resetSelection, routeState, urlWorkspaceId, userId]);

  useEffect(() => {
    if (!current) return;
    if (workspaceParamCount === 1 && urlWorkspaceId === current.workspaceId) {
      return;
    }
    writeWorkspaceParam(current.workspaceId);
  }, [current, writeWorkspaceParam, urlWorkspaceId, workspaceParamCount]);

  useEffect(() => {
    const previous = identity.current;
    if (previous === userId) return;
    identity.current = userId;
    resetSelection();
    setSwitchError(null);
    void client.cancelQueries({ queryKey: ["workspace", "detail", previous] });
  }, [client, resetSelection, userId]);

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

  function canContinue(ticket: number): boolean {
    if (switchSeq.current !== ticket) return false;
    if (isCurrentSessionUser(client, userId)) return true;
    resetSelection();
    return false;
  }

  const captureCreateScope = useCallback(
    (): CreateSubmitScope => ({ userId, ticket: switchSeq.current }),
    [userId],
  );

  function canNavigateAfterCreate(scope: CreateSubmitScope): boolean {
    if (!mounted.current) return false;
    if (scope.userId !== userId) return false;
    return canContinue(scope.ticket);
  }

  async function selectWorkspace(target: Workspace) {
    if (!current) return;
    if (pendingId === target.workspaceId) return;
    const selectingCurrent = target.workspaceId === current.workspaceId;
    if (selectingCurrent && pendingId === null && switchError === null) return;

    const ticket = switchSeq.current + 1;
    switchSeq.current = ticket;
    setSwitchError(null);
    setPendingId(selectingCurrent ? null : target.workspaceId);
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
      if (activeDetail.current === detail) activeDetail.current = null;
    }
  }

  async function handleCreated(created: Workspace, scope: CreateSubmitScope) {
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

  const wrap = (body: React.ReactNode, workspaceSlot?: React.ReactNode) => {
    if (!header) {
      return (
        <div className="flex flex-col gap-4">
          {workspaceSlot}
          {body}
        </div>
      );
    }
    return (
      <>
        <AppTopbar
          workspaceSlot={workspaceSlot}
          email={header.email}
          logoutPending={header.logoutPending}
          onLogout={header.onLogout}
          showNav
        />
        <main className="tp-page-shell">
          {header.logoutError ? (
            <Alert variant="destructive">
              <AlertDescription>{header.logoutError}</AlertDescription>
            </Alert>
          ) : null}
          {body}
        </main>
      </>
    );
  };

  if (list.isPending) {
    return wrap(
      <p role="status" className="text-muted-foreground text-sm">
        正在加载工作区…
      </p>,
    );
  }

  if (list.isError) {
    return wrap(
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
      </div>,
    );
  }

  if (!current) {
    return wrap(
      <WorkspaceCreateDialog
        userId={userId}
        required
        onClose={() => undefined}
        captureScope={captureCreateScope}
        onCreated={handleCreated}
      />,
    );
  }

  const switcher = (
    <WorkspaceSwitcher
      workspaces={workspaces}
      current={current}
      pendingId={pendingId}
      onCreate={() => setCreateOpen(true)}
      onSelect={(workspace) => void selectWorkspace(workspace)}
    />
  );

  return wrap(
    <>
      <WorkspaceSwitchError error={switchError} />
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
    </>,
    switcher,
  );
}
