import { useEffect, useRef, useState } from "react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import type { Workspace } from "./api";

export function WorkspaceSwitcher({
  workspaces,
  current,
  pendingId,
  onCreate,
  onSelect,
}: {
  workspaces: Workspace[];
  current: Workspace;
  pendingId: string | null;
  onCreate: () => void;
  onSelect: (workspace: Workspace) => void;
}) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    const onMouseDown = (event: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(event.target as Node)) {
        setOpen(false);
      }
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onMouseDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("mousedown", onMouseDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [open]);

  return (
    <div ref={rootRef} className="tp-ws-wrap" aria-label="工作区切换">
      <button
        type="button"
        className="tp-ws-trigger"
        aria-label={`切换工作区：${current.name}`}
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <span className="tp-ws-dot" aria-hidden="true" />
        <span>{current.name}</span>
        {pendingId ? (
          <span className="geek-muted geek-num text-[11px]">…</span>
        ) : null}
        <span className="geek-faint text-[10px]" aria-hidden="true">
          ▾
        </span>
      </button>
      {open ? (
        <div className="tp-popover" role="menu" aria-label="工作区列表">
          <div className="tp-pop-head">
            <span>工作区</span>
            <span className="geek-num">{workspaces.length}</span>
          </div>
          {workspaces.map((workspace) => {
            const selected = workspace.workspaceId === current.workspaceId;
            const pending = workspace.workspaceId === pendingId;
            return (
              <button
                key={workspace.workspaceId}
                type="button"
                className="tp-pop-item"
                aria-pressed={selected}
                disabled={pending}
                onClick={() => {
                  onSelect(workspace);
                  setOpen(false);
                }}
              >
                <span className="geek-num w-3 text-center" aria-hidden="true">
                  {selected ? "✓" : ""}
                </span>
                <span>
                  {workspace.name}
                  {pending ? " …" : ""}
                </span>
              </button>
            );
          })}
          <div className="tp-pop-divider" />
          <button
            type="button"
            className="tp-pop-item tp-pop-action"
            onClick={() => {
              setOpen(false);
              onCreate();
            }}
          >
            创建工作区
          </button>
        </div>
      ) : null}
    </div>
  );
}

export function WorkspaceSwitchError({ error }: { error: string | null }) {
  if (!error) return null;
  return (
    <Alert variant="destructive">
      <AlertDescription>{error}</AlertDescription>
    </Alert>
  );
}
