// Browser client for the gateway. Every call goes to the same origin, so the
// console works identically in development (Next rewrites /v1 to the gateway)
// and production (the gateway serves the console).

const SESSION_KEY = "ayeusann.session";

export type Session = { access: string; refresh: string };

export class ApiError extends Error {
  status: number;
  fields: Record<string, string>;
  constructor(message: string, status: number, fields: Record<string, string> = {}) {
    super(message);
    this.status = status;
    this.fields = fields;
  }
}

export function getSession(): Session | null {
  if (typeof window === "undefined") return null;
  try {
    const raw = window.localStorage.getItem(SESSION_KEY);
    return raw ? (JSON.parse(raw) as Session) : null;
  } catch {
    return null;
  }
}

export function setSession(s: Session | null) {
  try {
    if (s) window.localStorage.setItem(SESSION_KEY, JSON.stringify(s));
    else window.localStorage.removeItem(SESSION_KEY);
  } catch {
    // Private mode or blocked storage: the session lasts for this tab only.
  }
}

// One refresh at a time: parallel 401s share the same promise.
let refreshing: Promise<boolean> | null = null;

async function refreshSession(): Promise<boolean> {
  const s = getSession();
  if (!s?.refresh) return false;
  refreshing ??= (async () => {
    try {
      const res = await fetch("/v1/auth/refresh", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ refresh_token: s.refresh }),
      });
      if (!res.ok) return false;
      const data = await res.json();
      setSession({ access: data.access_token, refresh: data.refresh_token });
      return true;
    } catch {
      return false;
    } finally {
      setTimeout(() => (refreshing = null), 0);
    }
  })();
  return refreshing;
}

type Options = {
  method?: string;
  body?: unknown;
  auth?: boolean;
  // Adds an Idempotency-Key so a retried create cannot run twice.
  idempotent?: boolean;
};

export async function request<T>(path: string, opts: Options = {}, retried = false): Promise<T> {
  const { method = "GET", body, auth = true, idempotent = false } = opts;
  const headers: Record<string, string> = {};
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const s = auth ? getSession() : null;
  if (s) headers["Authorization"] = `Bearer ${s.access}`;
  if (idempotent) headers["Idempotency-Key"] = crypto.randomUUID();

  let res: Response;
  try {
    res = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  } catch {
    throw new ApiError("Can't reach the server. Check that the platform is running.", 0);
  }

  if (res.status === 401 && s && !retried) {
    if (await refreshSession()) return request<T>(path, opts, true);
    setSession(null);
    window.dispatchEvent(new Event("session-expired"));
  }

  const text = await res.text();
  let data: any = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = { detail: text };
    }
  }
  if (!res.ok) {
    const message =
      data?.detail ?? data?.error?.message ?? (typeof data?.error === "string" ? data.error : null) ?? res.statusText;
    throw new ApiError(message, res.status, data?.fields ?? {});
  }
  return data as T;
}

export const api = {
  get: <T>(path: string, auth = true) => request<T>(path, { auth }),
  post: <T>(path: string, body?: unknown, opts: Omit<Options, "method" | "body"> = {}) =>
    request<T>(path, { method: "POST", body: body ?? {}, ...opts }),
  patch: <T>(path: string, body: unknown) => request<T>(path, { method: "PATCH", body }),
  del: <T>(path: string) => request<T>(path, { method: "DELETE" }),
};
