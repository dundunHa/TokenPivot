import { useEffect, useMemo, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useSearchParams } from "react-router";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { OverviewPage } from "@/features/overview/OverviewPage";
import { ApiError } from "@/lib/api";
import type { Workspace } from "./api";
import { WorkspaceCreateDialog } from "./WorkspaceCreateDialog";
import { WorkspaceSwitcher } from "./WorkspaceSwitcher";
import { useWorkspaceList, workspaceDetailQuery } from "./queries";

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
  const switchSeq = useRef(0);

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

  function cancelWorkspaceBusiness(workspaceId: string): Promise<void> {
    return client.cancelQueries({
      predicate: (query) => query.meta?.workspaceId === workspaceId,
    });
  }

  async function selectWorkspace(target: Workspace) {
    if (!current || target.workspaceId === current.workspaceId) return;
    if (pendingId === target.workspaceId) return;
    const ticket = switchSeq.current + 1;
    switchSeq.current = ticket;
    setSwitchError(null);
    setPendingId(target.workspaceId);
    // 取消上一个详情验证；新选择到来时旧请求不会再发或再写 URL。
    await client.cancelQueries({
      queryKey: ["workspace", "detail", userId],
    });
    if (switchSeq.current !== ticket) return;
    try {
      await client.fetchQuery(workspaceDetailQuery(userId, target.workspaceId));
      if (switchSeq.current !== ticket) return;
      await cancelWorkspaceBusiness(current.workspaceId);
      if (switchSeq.current !== ticket) return;
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
      if (switchSeq.current !== ticket) return;
      setPendingId(null);
      setSwitchError(
        error instanceof ApiError && error.status === 403
          ? `无法切换到「${target.name}」：没有访问权限。`
          : "切换失败，请重试。",
      );
    }
  }

  async function handleCreated(created: Workspace) {
    const ticket = switchSeq.current + 1;
    switchSeq.current = ticket;
    await client.cancelQueries({ queryKey: ["workspace", "detail", userId] });
    if (current) await cancelWorkspaceBusiness(current.workspaceId);
    if (switchSeq.current !== ticket) return;
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
          onCreated={handleCreated}
        />
      ) : null}
    </div>
  );
}
