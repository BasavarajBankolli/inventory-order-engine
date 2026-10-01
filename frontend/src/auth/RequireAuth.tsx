import type { ReactNode } from "react";
import { Navigate, useLocation } from "react-router";
import { EmptyState, Spinner } from "../components/ui";
import { useAuth } from "./AuthContext";

/**
 * Route guard. Not logged in -> /login (and back here afterwards).
 * `admin`: logged in but not an ADMIN -> "no access" message.
 *
 * This only improves the UX: the BACKEND enforces the same rules on every
 * request (401/403), so hiding a page here is never the security boundary.
 */
export function RequireAuth({ children, admin = false }: { children: ReactNode; admin?: boolean }) {
  const { user, isAdmin, initializing } = useAuth();
  const location = useLocation();

  if (initializing) return <Spinner label="Checking your session…" />;
  if (!user) {
    const next = encodeURIComponent(location.pathname + location.search);
    return <Navigate to={`/login?next=${next}`} replace />;
  }
  if (admin && !isAdmin) {
    return <EmptyState title="Admins only" text="Your account does not have access to this page." />;
  }
  return <>{children}</>;
}
