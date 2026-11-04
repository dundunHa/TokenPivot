import { useMemo } from "react";
import { FiltersGate } from "@/features/overview/OverviewPage";
import { OverviewFilterBar } from "@/features/overview/OverviewFilterBar";
import {
  useProjects,
  type QueryScope,
} from "@/features/overview/overviewQueries";
import { useUsageOptions } from "@/features/workspace/queries";
import { RateKpi } from "./RateKpi";
import { RequestLogPanel } from "./RequestLogPanel";

/** 请求明细独立页：筛选与总览共用同一组 URL 参数，这里只开放供应商/模型/项目。 */
export function RequestsPage({
  userId,
  workspaceId,
}: {
  userId: string;
  workspaceId: string;
}) {
  const scope = useMemo<QueryScope>(
    () => ({ userId, workspaceId }),
    [userId, workspaceId],
  );
  return (
    <FiltersGate>
      {(filters, update) => (
        <RequestsContent scope={scope} filters={filters} update={update} />
      )}
    </FiltersGate>
  );
}

function RequestsContent({
  scope,
  filters,
  update,
}: {
  scope: QueryScope;
  filters: Parameters<typeof RequestLogPanel>[0]["filters"];
  update: Parameters<typeof OverviewFilterBar>[0]["onChange"];
}) {
  const options = useUsageOptions(scope.userId, scope.workspaceId);
  const projects = useProjects(scope);
  const asOption = (v: string) => ({ value: v, label: v });
  const projectOptions = [
    ...(projects.data ?? []).map((p) => ({
      value: p.projectId,
      label: p.archived ? `${p.name}（已归档）` : p.name,
    })),
    { value: "unassigned", label: "未归属" },
  ];
  return (
    <div className="geek-main">
      <OverviewFilterBar
        dimensionsOnly
        filters={filters}
        providerOptions={(options.data?.providers ?? []).map(asOption)}
        modelOptions={(options.data?.models ?? []).map(asOption)}
        projectOptions={projectOptions}
        optionsError={
          options.isError ? "筛选选项加载失败，暂时无法编辑供应商/模型。" : null
        }
        projectsError={
          projects.isError ? "项目列表加载失败，暂时无法编辑项目。" : null
        }
        onChange={update}
      />
      <div className="geek-kpis geek-kpis-single">
        <RateKpi scope={scope} filters={filters} />
      </div>
      <div className="geek-card">
        <RequestLogPanel scope={scope} filters={filters} />
      </div>
    </div>
  );
}
