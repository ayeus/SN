"use client";

import { createContext, useCallback, useContext, useEffect, useMemo, useState } from "react";
import { api, getSession, setSession } from "./api";
import type { Me } from "./types";

type AuthResponse = { access_token: string; refresh_token: string; promotional_credit?: string };

type AuthState = {
  me: Me | null;
  loading: boolean;
  login: (email: string, password: string) => Promise<void>;
  signup: (input: { name: string; email: string; password: string; country: string }) => Promise<string | undefined>;
  logout: () => Promise<void>;
  reload: () => Promise<void>;
};

const Ctx = createContext<AuthState | null>(null);

export function AuthProvider({ children }: { children: React.ReactNode }) {
  const [me, setMe] = useState<Me | null>(null);
  const [loading, setLoading] = useState(true);

  const reload = useCallback(async () => {
    if (!getSession()) {
      setMe(null);
      setLoading(false);
      return;
    }
    try {
      setMe(await api.get<Me>("/v1/auth/me"));
    } catch {
      setMe(null);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    reload();
    const expired = () => setMe(null);
    window.addEventListener("session-expired", expired);
    return () => window.removeEventListener("session-expired", expired);
  }, [reload]);

  const value = useMemo<AuthState>(
    () => ({
      me,
      loading,
      reload,
      async login(email, password) {
        const r = await api.post<AuthResponse>("/v1/auth/login", { email, password }, { auth: false });
        setSession({ access: r.access_token, refresh: r.refresh_token });
        await reload();
      },
      async signup(input) {
        const r = await api.post<AuthResponse>("/v1/auth/signup", input, { auth: false });
        setSession({ access: r.access_token, refresh: r.refresh_token });
        await reload();
        return r.promotional_credit;
      },
      async logout() {
        const s = getSession();
        try {
          if (s) await api.post("/v1/auth/logout", { refresh_token: s.refresh });
        } catch {
          // Signing out locally is what matters to the user.
        }
        setSession(null);
        setMe(null);
      },
    }),
    [me, loading, reload],
  );

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useAuth(): AuthState {
  const v = useContext(Ctx);
  if (!v) throw new Error("useAuth outside AuthProvider");
  return v;
}
