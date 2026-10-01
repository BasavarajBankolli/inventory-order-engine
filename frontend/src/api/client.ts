import type { ApiErrorBody } from "../types/api";

/** Backend base URL from VITE_API_BASE_URL (baked in at build time). */
export const API_BASE_URL = (import.meta.env.VITE_API_BASE_URL || "http://localhost:8080").replace(/\/+$/, "");

/**
 * Every failed request becomes an ApiError with the backend's error code,
 * message, per-field validation errors and request id (for support).
 */
export class ApiError extends Error {
  readonly status: number; // HTTP status; 0 = network error (server down, CORS, offline)
  readonly code: string; // e.g. OUT_OF_STOCK, VALIDATION_ERROR
  readonly fields: Record<string, string>;
  readonly requestId?: string;

  constructor(status: number, code: string, message: string, fields: Record<string, string> = {}, requestId?: string) {
    // Backend messages start lowercase ("invalid email or password").
    super(message.charAt(0).toUpperCase() + message.slice(1));
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.fields = fields;
    this.requestId = requestId;
  }
}

// --- auth hooks, set by AuthContext -------------------------------------------

let accessToken: string | null = null;
let onUnauthorized: (() => void) | null = null;

export function setAccessToken(token: string | null) {
  accessToken = token;
}

/** Called when an authenticated request gets 401 (token expired/revoked). */
export function setUnauthorizedHandler(handler: (() => void) | null) {
  onUnauthorized = handler;
}

// --- request ---------------------------------------------------------------------

export interface RequestOptions {
  body?: unknown;
  query?: Record<string, string | number | undefined>;
  headers?: Record<string, string>;
  /** Send the bearer token (default true when logged in). */
  auth?: boolean;
  signal?: AbortSignal;
}

export interface ApiResponse<T> {
  data: T;
  status: number;
  headers: Headers;
}

/** Low-level request that also returns status and headers. */
export async function request<T>(method: string, path: string, opts: RequestOptions = {}): Promise<ApiResponse<T>> {
  const url = new URL(API_BASE_URL + path);
  for (const [k, v] of Object.entries(opts.query ?? {})) {
    if (v !== undefined && v !== "") url.searchParams.set(k, String(v));
  }

  const headers: Record<string, string> = { Accept: "application/json", ...opts.headers };
  if (opts.body !== undefined) headers["Content-Type"] = "application/json";
  const sendToken = (opts.auth ?? true) && accessToken;
  if (sendToken) headers.Authorization = `Bearer ${accessToken}`;

  let res: Response;
  try {
    res = await fetch(url, {
      method,
      headers,
      body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
      signal: opts.signal,
    });
  } catch (err) {
    if ((err as Error).name === "AbortError") throw err;
    throw new ApiError(
      0,
      "NETWORK_ERROR",
      `Cannot reach the server at ${API_BASE_URL}. Check your connection or try again shortly.`,
    );
  }

  const text = res.status === 204 ? "" : await res.text();
  const json: unknown = text ? safeParse(text) : undefined;

  if (!res.ok) {
    const body = (json as ApiErrorBody | undefined)?.error;
    let message = body?.message ?? `Request failed (${res.status})`;
    if (res.status === 429) {
      const retry = res.headers.get("Retry-After");
      message = `Too many requests. Please wait${retry ? ` ${retry} seconds` : " a moment"} and try again.`;
    }
    const error = new ApiError(
      res.status,
      body?.code ?? "HTTP_" + res.status,
      message,
      body?.fields ?? {},
      body?.request_id ?? res.headers.get("X-Request-ID") ?? undefined,
    );
    if (res.status === 401 && sendToken && onUnauthorized) onUnauthorized();
    throw error;
  }

  return { data: json as T, status: res.status, headers: res.headers };
}

function safeParse(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    return undefined;
  }
}

/** Convenience wrappers returning only the body. */
export const api = {
  get: <T>(path: string, opts?: RequestOptions) => request<T>("GET", path, opts).then((r) => r.data),
  post: <T>(path: string, body?: unknown, opts?: RequestOptions) =>
    request<T>("POST", path, { ...opts, body }).then((r) => r.data),
  patch: <T>(path: string, body?: unknown, opts?: RequestOptions) =>
    request<T>("PATCH", path, { ...opts, body }).then((r) => r.data),
  delete: (path: string, opts?: RequestOptions) => request<void>("DELETE", path, opts).then(() => undefined),
};

/** Human-friendly message for any thrown value. */
export function errorMessage(err: unknown): string {
  if (err instanceof ApiError) return err.message;
  if (err instanceof Error) return err.message;
  return "Something went wrong.";
}
