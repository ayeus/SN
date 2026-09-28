"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError } from "./api";

type Result<T> = {
  data: T | null;
  error: ApiError | null;
  loading: boolean;
  reload: () => Promise<void>;
};

/**
 * Loads data and, when `every` is set, keeps it fresh by polling while the tab
 * is visible. Live views (deployment progress, host status) use this.
 */
export function useData<T>(load: (() => Promise<T>) | null, deps: unknown[], every?: number): Result<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [loading, setLoading] = useState(true);
  const loadRef = useRef(load);
  loadRef.current = load;

  const reload = useCallback(async () => {
    const fn = loadRef.current;
    if (!fn) return;
    try {
      setData(await fn());
      setError(null);
    } catch (e) {
      setError(e instanceof ApiError ? e : new ApiError(String(e), 0));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    setLoading(true);
    reload();
    if (!every) return;
    const id = window.setInterval(() => {
      if (document.visibilityState === "visible") reload();
    }, every);
    return () => window.clearInterval(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);

  return { data, error, loading, reload };
}

export function useCopy(): [string | null, (text: string, id?: string) => void] {
  const [copied, setCopied] = useState<string | null>(null);
  const copy = useCallback((text: string, id = text) => {
    navigator.clipboard?.writeText(text).catch(() => {});
    setCopied(id);
    window.setTimeout(() => setCopied((c) => (c === id ? null : c)), 1600);
  }, []);
  return [copied, copy];
}
