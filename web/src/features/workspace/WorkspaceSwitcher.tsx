import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import type { Workspace } from "./api";

export function WorkspaceSwitcher({
  workspaces,
  current,
  pendingId,
  error,
  onCreate,
  onSelect,
}: {
  workspaces: Workspace[];
  current: Workspace;
  pendingId: string | null;
  error: string | null;
  onCreate: () => void;
  onSelect: (workspace: Workspace) => void;
}) {
  return (
    <section className="flex flex-col gap-2" aria-label="工作区切换">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-subtle text-sm">空间</span>
        <strong className="text-sm">{current.name}</strong>
        <div className="ml-auto flex flex-wrap gap-2">
          {workspaces.map((workspace) => {
            const selected = workspace.workspaceId === current.workspaceId;
            const pending = workspace.workspaceId === pendingId;
            return (
              <Button
                key={workspace.workspaceId}
                type="button"
                size="inline"
                variant={selected ? "default" : "outline"}
                className="px-2 py-1"
                aria-pressed={selected}
                disabled={pending}
                onClick={() => onSelect(workspace)}
              >
                {workspace.name}
                {pending ? " …" : ""}
              </Button>
            );
          })}
          <Button type="button" variant="outline" onClick={onCreate}>
            创建工作区
          </Button>
        </div>
      </div>
      {error ? (
        <Alert variant="destructive">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      ) : null}
    </section>
  );
}
