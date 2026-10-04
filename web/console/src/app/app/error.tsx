"use client";

import { useEffect } from "react";
import { Button, ButtonLink, Notice } from "@/components/ui";

// Keeps the console shell (navigation) usable when one page fails.
export default function ConsoleError({ error, reset }: { error: Error & { digest?: string }; reset: () => void }) {
  useEffect(() => {
    console.error("[console] page error", error);
  }, [error]);
  return (
    <div className="max-w-[640px]">
      <h1 className="mb-3 text-[22px] font-semibold">This page hit a problem</h1>
      <Notice tone="error" className="mb-5">
        {error.message || "Something went wrong while showing this page."}
        {error.digest && <span className="mt-1 block text-[12px] text-muted">Reference: {error.digest}</span>}
      </Notice>
      <div className="flex gap-2">
        <Button onClick={reset}>Try again</Button>
        <ButtonLink href="/app" variant="secondary">
          Go to overview
        </ButtonLink>
      </div>
    </div>
  );
}
