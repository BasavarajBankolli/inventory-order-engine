import type { ReactNode } from "react";
import { Link } from "react-router";
import { ApiError, errorMessage } from "../api/client";
import { statusTone } from "../utils/format";

export function Spinner({ label = "Loading…" }: { label?: string }) {
  return (
    <div className="spinner-wrap" role="status" aria-live="polite">
      <span className="spinner" aria-hidden="true" />
      <span>{label}</span>
    </div>
  );
}

/** Shows an API error the same way everywhere, with a retry button and the
 *  request id (support can find the exact server logs with it). */
export function ErrorAlert({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  const requestId = error instanceof ApiError ? error.requestId : undefined;
  return (
    <div className="alert alert-danger" role="alert">
      <div>
        <strong>{errorMessage(error)}</strong>
        {requestId && <div className="muted small">Reference: {requestId}</div>}
      </div>
      {onRetry && (
        <button className="btn btn-sm" onClick={onRetry}>
          Try again
        </button>
      )}
    </div>
  );
}

export function Notice({ tone = "info", children }: { tone?: "info" | "success" | "warning" | "danger"; children: ReactNode }) {
  return (
    <div className={`alert alert-${tone}`} role={tone === "danger" ? "alert" : "status"}>
      <div>{children}</div>
    </div>
  );
}

export function EmptyState({ title, text, action }: { title: string; text?: string; action?: ReactNode }) {
  return (
    <div className="empty">
      <h3>{title}</h3>
      {text && <p className="muted">{text}</p>}
      {action}
    </div>
  );
}

export function StatusBadge({ status, label }: { status: string; label?: string }) {
  return <span className={`badge badge-${statusTone(status)}`}>{label ?? status.replaceAll("_", " ")}</span>;
}

export function PageHeader({ title, subtitle, actions }: { title: string; subtitle?: ReactNode; actions?: ReactNode }) {
  return (
    <div className="page-header">
      <div>
        <h1>{title}</h1>
        {subtitle && <p className="muted">{subtitle}</p>}
      </div>
      {actions && <div className="page-actions">{actions}</div>}
    </div>
  );
}

export function Pagination({
  total,
  limit,
  offset,
  onChange,
}: {
  total: number;
  limit: number;
  offset: number;
  onChange: (offset: number) => void;
}) {
  if (total <= limit) return null;
  const page = Math.floor(offset / limit) + 1;
  const pages = Math.ceil(total / limit);
  return (
    <nav className="pagination" aria-label="Pagination">
      <button className="btn btn-sm" disabled={page <= 1} onClick={() => onChange(offset - limit)}>
        ← Previous
      </button>
      <span className="muted">
        Page {page} of {pages} · {total} total
      </span>
      <button className="btn btn-sm" disabled={page >= pages} onClick={() => onChange(offset + limit)}>
        Next →
      </button>
    </nav>
  );
}

/** A labelled form control with the backend's per-field validation error. */
export function Field({
  label,
  error,
  hint,
  children,
}: {
  label: string;
  error?: string;
  hint?: string;
  children: ReactNode;
}) {
  return (
    <label className={`field ${error ? "field-invalid" : ""}`}>
      <span className="field-label">{label}</span>
      {children}
      {error ? <span className="field-error">{error}</span> : hint && <span className="field-hint">{hint}</span>}
    </label>
  );
}

export function BackLink({ to, children }: { to: string; children: ReactNode }) {
  return (
    <Link to={to} className="back-link">
      ← {children}
    </Link>
  );
}
