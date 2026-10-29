import type { ReactNode } from "react";

export function AuthLayout({
  title,
  description,
  children,
}: {
  title: string;
  description: string;
  children: ReactNode;
}) {
  return (
    <main className="mx-auto flex min-h-screen w-full max-w-sm flex-col justify-center gap-6 px-6 py-10">
      <header className="flex flex-col gap-2">
        <p className="text-primary text-sm">TokenPivot</p>
        <h1 className="text-xl font-semibold">{title}</h1>
        <p className="text-muted-foreground text-sm">{description}</p>
      </header>
      <div className="border-divider border-t" />
      {children}
    </main>
  );
}
