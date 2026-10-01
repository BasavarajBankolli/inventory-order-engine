import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import { setAccessToken, setUnauthorizedHandler } from "../api/client";
import { authApi } from "../api/services";
import type { User } from "../types/api";

// The backend is stateless (JWT): logging in returns a signed token that is
// sent as "Authorization: Bearer <token>" on every request. We keep it in
// localStorage so a page reload keeps you logged in, and drop it when it
// expires or the backend answers 401.

const STORAGE_KEY = "stockflow.auth";

interface StoredAuth {
  token: string;
  expiresAt: string;
  user: User;
}

interface AuthContextValue {
  user: User | null;
  isAdmin: boolean;
  /** true while a stored session is being checked on page load */
  initializing: boolean;
  login: (email: string, password: string) => Promise<User>;
  register: (name: string, email: string, password: string) => Promise<User>;
  logout: () => void;
}

const AuthContext = createContext<AuthContextValue | null>(null);

function readStored(): StoredAuth | null {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return null;
    const stored = JSON.parse(raw) as StoredAuth;
    if (new Date(stored.expiresAt).getTime() <= Date.now()) return null; // expired
    return stored;
  } catch {
    return null;
  }
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [session, setSession] = useState<StoredAuth | null>(() => {
    const stored = readStored();
    setAccessToken(stored?.token ?? null); // before the first request goes out
    return stored;
  });
  const [initializing, setInitializing] = useState(session !== null);

  const logout = useCallback(() => {
    localStorage.removeItem(STORAGE_KEY);
    setAccessToken(null);
    setSession(null);
  }, []);

  const saveSession = useCallback((s: StoredAuth) => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(s));
    setAccessToken(s.token);
    setSession(s);
  }, []);

  // Any 401 on an authenticated request = the token is no longer valid.
  useEffect(() => {
    setUnauthorizedHandler(logout);
    return () => setUnauthorizedHandler(null);
  }, [logout]);

  // On page load, confirm the stored token still works and refresh the
  // user (e.g. an admin promotion since the last visit shows up only after
  // re-login, because the role is inside the token).
  useEffect(() => {
    if (!session) return;
    authApi
      .me()
      .then((user) => saveSession({ ...session, user }))
      .catch(() => {
        /* 401 already logged us out; network errors keep the session */
      })
      .finally(() => setInitializing(false));
  }, []); // run once on mount

  // Log out automatically when the token expires.
  useEffect(() => {
    if (!session) return;
    const ms = new Date(session.expiresAt).getTime() - Date.now();
    // setTimeout overflows above ~24.8 days (2^31-1 ms) and would fire at once.
    const id = setTimeout(logout, Math.min(Math.max(ms, 0), 2_147_483_647));
    return () => clearTimeout(id);
  }, [session, logout]);

  const login = useCallback(
    async (email: string, password: string) => {
      const res = await authApi.login(email, password);
      saveSession({ token: res.access_token, expiresAt: res.expires_at, user: res.user });
      return res.user;
    },
    [saveSession],
  );

  const register = useCallback(
    async (name: string, email: string, password: string) => {
      await authApi.register(name, email, password);
      return login(email, password); // registration does not return a token
    },
    [login],
  );

  const value = useMemo<AuthContextValue>(
    () => ({
      user: session?.user ?? null,
      isAdmin: session?.user.role === "ADMIN",
      initializing,
      login,
      register,
      logout,
    }),
    [session, initializing, login, register, logout],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used inside <AuthProvider>");
  return ctx;
}
