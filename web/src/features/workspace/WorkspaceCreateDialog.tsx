import { useEffect, useRef, useState } from "react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Field,
  FieldDescription,
  FieldError,
  FieldLabel,
} from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { ApiError } from "@/lib/api";
import { trimWorkspaceName, workspaceNameProblem, type Workspace } from "./api";
import { useCreateWorkspace } from "./queries";

function createErrorMessage(error: unknown): string | null {
  if (!(error instanceof ApiError)) return "请求失败，请重试。";
  if (error.status === 401) return null;
  switch (error.code) {
    case "WORKSPACE_NAME_CONFLICT":
      return "名称已存在，请换一个。";
    case "WORKSPACE_LIMIT_REACHED":
      return "已达到 2 个工作区上限。";
    case "INVALID_ARGUMENT":
      return "名称不符合规则，请检查后重试。";
    case "PERMISSION_DENIED":
      return "没有权限创建工作区。";
    case "NETWORK":
      return "网络异常，请检查连接后重试。";
    default:
      return error.status >= 500
        ? "服务暂不可用，请稍后重试。"
        : "创建失败，请重试。";
  }
}

export function WorkspaceCreateDialog({
  userId,
  required,
  onClose,
  onCreated,
}: {
  userId: string;
  /** 零空间时不可关闭：没有关闭按钮，Escape 与遮罩也不生效。 */
  required: boolean;
  onClose: () => void;
  onCreated: (workspace: Workspace) => void | Promise<void>;
}) {
  const create = useCreateWorkspace(userId);
  const [name, setName] = useState("");
  const [problem, setProblem] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const inFlight = useRef(false);

  useEffect(() => {
    inputRef.current?.focus();
  }, []);

  useEffect(() => {
    if (required || create.isPending) return;
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") onClose();
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [required, onClose, create.isPending]);

  async function submit() {
    if (inFlight.current) return;
    const invalid = workspaceNameProblem(name);
    if (invalid) {
      setProblem(invalid);
      return;
    }
    inFlight.current = true;
    setProblem(null);
    try {
      const created = await create.mutateAsync(trimWorkspaceName(name));
      await onCreated(created);
    } catch {
      // 失败原因由 mutation.error 渲染；输入与 modal 在此保留。
    } finally {
      inFlight.current = false;
    }
  }

  const serverError = createErrorMessage(create.error);
  const invalid = problem !== null;

  return (
    <div
      className="bg-background/80 fixed inset-0 z-50 flex items-center justify-center p-4"
      onMouseDown={(event) => {
        if (
          !required &&
          !create.isPending &&
          event.target === event.currentTarget
        ) {
          onClose();
        }
      }}
    >
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby="workspace-create-title"
        className="border-border bg-card flex w-full max-w-md flex-col gap-4 rounded-lg border p-6 shadow-lg"
      >
        <div className="flex flex-col gap-1">
          <h2 id="workspace-create-title" className="text-lg font-semibold">
            创建工作区
          </h2>
          <p className="text-muted-foreground text-sm">
            工作区用于隔离项目、来源和用量数据。每个账户最多创建 2 个。
          </p>
        </div>
        <form
          className="flex flex-col gap-4"
          noValidate
          aria-busy={create.isPending}
          onSubmit={(event) => {
            event.preventDefault();
            void submit();
          }}
        >
          <Field data-invalid={invalid || serverError !== null}>
            <FieldLabel htmlFor="workspace-name">名称</FieldLabel>
            <Input
              ref={inputRef}
              id="workspace-name"
              name="name"
              value={name}
              autoComplete="off"
              disabled={create.isPending}
              aria-invalid={invalid || serverError !== null}
              aria-describedby="workspace-name-help"
              onChange={(event) => {
                setName(event.target.value);
                setProblem(null);
                create.reset();
              }}
            />
            <FieldDescription id="workspace-name-help">
              首尾空白会被去除；最多 80 个字符且不超过 256 字节。
            </FieldDescription>
            {problem ? <FieldError>{problem}</FieldError> : null}
          </Field>
          {serverError ? (
            <Alert variant="destructive">
              <AlertDescription>{serverError}</AlertDescription>
            </Alert>
          ) : null}
          <div className="flex justify-end gap-2">
            {required ? null : (
              <Button
                type="button"
                variant="outline"
                disabled={create.isPending}
                onClick={onClose}
              >
                取消
              </Button>
            )}
            <Button type="submit" disabled={create.isPending}>
              {create.isPending ? "正在创建…" : "创建"}
            </Button>
          </div>
        </form>
      </div>
    </div>
  );
}
