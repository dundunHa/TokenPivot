import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useSearchParams } from "react-router";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { OverviewPage } from "@/features/overview/OverviewPage";
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

/**
 * 工作区容器：列表就绪前或为空时不挂载任何业务页面。
 * 当前空间只来自 URL 的 `workspace` 参数；参数缺失或不属于当前用户时回落到服务端首项。
 */
export function WorkspaceHome({ userId }: { userId: string }) {
  const client = useQueryClient();
  const list = useWorkspaceList(userId);
  const [params, setParams] = useSearchParams();
  const [createOpen, setCreateOpen] = useState(false);
  const [pendingId, setPendingId] = useState<string | null>(null);
  const [switchError, setSwitchError] = useState<string | null>(null);
  /** 选择序号：每次最新选择自增，旧回调据此失效。 */
  const switchSeq = useRef(0);
  /** 进行中切换的目标空间；URL 被外部改到别的空间时据此作废旧选择。 */
  const switchTarget = useRef<string | null>(null);
  /** 本容器实例当前的身份；身份变化后旧选择不得再导航。 */
  const identity = useRef(userId);
  /** 挂载状态：卸载后的迟到回调只返回，不再设置状态或 URL。 */
  const mounted = useRef(true);
  /** 本容器发起、仍在等待的详情验证；卸载时只取消这一条。 */
  const activeDetail = useRef<{ userId: string; workspaceId: string } | null>(
    null,
  );
  /** 最新 URL 选择，供创建完成后判断期间是否发生过外部导航。 */
  const urlSelection = useRef<string | null>(null);

  /** 作废进行中的选择：递增序号、清掉 pending，旧回调此后一律提前返回。 */
  const resetSelection = useCallback(() => {
    switchSeq.current += 1;
    switchTarget.current = null;
    setPendingId(null);
  }, []);

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

  // URL 无有效成员工作区时规范化到当前选择，其他查询参数原样保留。
  useEffect(() => {
    if (!current) return;
    if (workspaceParamCount === 1 && urlWorkspaceId === current.workspaceId) {
      return;
    }
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        next.set("workspace", current.workspaceId);
        return next;
      },
      { replace: true },
    );
  }, [current, setParams, urlWorkspaceId, workspaceParamCount]);

  // 身份变化：旧身份进行中的选择作废，取消其详情验证，迟到的成功不得再改 URL。
  useEffect(() => {
    const previous = identity.current;
    if (previous === userId) return;
    identity.current = userId;
    resetSelection();
    setSwitchError(null);
    void client.cancelQueries({ queryKey: ["workspace", "detail", previous] });
  }, [client, resetSelection, userId]);

  // URL 选择的最新值；创建完成时据此识别期间发生的外部导航。
  useEffect(() => {
    urlSelection.current = urlWorkspaceId;
  }, [urlWorkspaceId]);

  // 卸载后旧回调同样作废：离开的容器不得再设置 URL 或状态。
  // 只取消本容器发起的那条详情验证，不动其他容器或新身份的任务。
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      switchSeq.current += 1;
      switchTarget.current = null;
      const detail = activeDetail.current;
      activeDetail.current = null;
      if (detail === null) return;
      void client.cancelQueries({
        queryKey: ["workspace", "detail", detail.userId, detail.workspaceId],
      });
    };
  }, [client]);

  // URL 被浏览器前进/后退或其他入口改到别的空间：先到的新位置优先，进行中的切换作废。
  useEffect(() => {
    const target = switchTarget.current;
    if (target === null || urlWorkspaceId === target) return;
    resetSelection();
    void client.cancelQueries({ queryKey: ["workspace", "detail", userId] });
  }, [client, resetSelection, urlWorkspaceId, userId]);

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

  /** 创建提交瞬间的选择与位置；返回后凭它判断是否还能导航。 */
  const captureCreateScope = useCallback(
    (): CreateSubmitScope => ({
      userId,
      ticket: switchSeq.current,
      urlWorkspaceId: urlSelection.current,
    }),
    [userId],
  );

  /** 创建完成时仍可导航：容器挂载、提交身份未变、选择未被接管、URL 未被外部改动。 */
  function canNavigateAfterCreate(
    scope: CreateSubmitScope,
    created: Workspace,
  ): boolean {
    if (!mounted.current) return false;
    if (scope.userId !== userId) return false;
    if (!canContinue(scope.ticket)) return false;
    const urlNow = urlSelection.current;
    // 提交时的位置未变，或已经落在新建空间上（零空间规范化），才不算外部导航。
    return urlNow === scope.urlWorkspaceId || urlNow === created.workspaceId;
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
    switchTarget.current = selectingCurrent ? null : target.workspaceId;
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
      switchTarget.current = null;
      setParams(
        (prev) => {
          const next = new URLSearchParams(prev);
          next.set("workspace", target.workspaceId);
          return next;
        },
        { replace: true },
      );
      setPendingId(null);
    } catch (error) {
      if (!canContinue(ticket)) return;
      switchTarget.current = null;
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
    // 创建已成功、列表已重查；此后只在提交时的容器、身份、选择与位置都仍有效时导航。
    if (!canNavigateAfterCreate(scope, created)) return;
    await client.cancelQueries({ queryKey: ["workspace", "detail", userId] });
    if (!canNavigateAfterCreate(scope, created)) return;
    if (current) await cancelWorkspaceBusiness(current.workspaceId);
    if (!canNavigateAfterCreate(scope, created)) return;
    setPendingId(null);
    setSwitchError(null);
    setCreateOpen(false);
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        next.set("workspace", created.workspaceId);
        return next;
      },
      { replace: true },
    );
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
    <div className="flex flex-col gap-6">
      <WorkspaceSwitcher
        workspaces={workspaces}
        current={current}
        pendingId={pendingId}
        error={switchError}
        onCreate={() => setCreateOpen(true)}
        onSelect={(workspace) => void selectWorkspace(workspace)}
      />
      <OverviewPage
        key={current.workspaceId}
        userId={userId}
        workspaceId={current.workspaceId}
        workspaceName={current.name}
      />
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
