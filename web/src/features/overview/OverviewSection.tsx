import type { ReactNode } from "react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { describeError } from "./overviewApi";
import { formatInstant } from "./overviewView";

interface QueryLike {
  isPending: boolean;
  isError: boolean;
  error: unknown;
  isFetching: boolean;
  dataUpdatedAt: number;
  refetch: () => unknown;
  data: unknown;
}

/** 分区状态：加载骨架、错误 + 重试、同键后台刷新失败时保留旧数据并提示。 */
export function Section({
  title,
  query,
  timezone,
  children,
  className,
}: {
  title: string;
  query: QueryLike;
  timezone: string;
  children: ReactNode;
  className?: string;
}) {
  let body: ReactNode;
  if (query.isPending) {
    body = (
      <div
        role="status"
        aria-label={`${title}加载中`}
        className="bg-accent h-24 animate-pulse rounded-md"
      />
    );
  } else if (query.data === undefined && query.isError) {
    const { kind, message } = describeError(query.error);
    body = (
      <div className="flex flex-col gap-2">
        <Alert variant="destructive">
          <AlertDescription>{message}</AlertDescription>
        </Alert>
        {kind === "forbidden" ? null : (
          <Button
            variant="outline"
            className="self-start"
            onClick={() => void query.refetch()}
          >
            重试
          </Button>
        )}
      </div>
    );
  } else {
    body = (
      <>
        {query.isError ? (
          <p role="alert" className="text-destructive mb-2 text-xs">
            更新失败，展示上次成功结果（
            {formatInstant(
              new Date(query.dataUpdatedAt).toISOString(),
              timezone,
              true,
            )}
            ）
          </p>
        ) : null}
        {children}
      </>
    );
  }
  return (
    <section aria-label={title} className={className}>
      {body}
    </section>
  );
}
