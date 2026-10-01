// One function per backend endpoint. Pages call these, never fetch() directly.
import { API_BASE_URL, api, request } from "./client";
import type {
  Inventory,
  LoginResponse,
  Order,
  OrderStatus,
  Page,
  PayResult,
  Product,
  ProductSort,
  ProductStatus,
  ReadyStatus,
  SimulatedOutcome,
  User,
} from "../types/api";

// --- auth & users --------------------------------------------------------------

export const authApi = {
  login: (email: string, password: string) =>
    api.post<LoginResponse>("/api/v1/auth/login", { email, password }, { auth: false }),
  register: (name: string, email: string, password: string) =>
    api.post<User>("/api/v1/auth/register", { name, email, password }, { auth: false }),
  me: () => api.get<User>("/api/v1/users/me"),
};

// --- products --------------------------------------------------------------------

export interface ProductQuery {
  q?: string;
  status?: Exclude<ProductStatus, "ARCHIVED">;
  sort?: ProductSort;
  limit?: number;
  offset?: number;
}

export interface ProductInput {
  sku: string;
  name: string;
  description: string;
  price: number; // minor units
  currency: string;
  status: Exclude<ProductStatus, "ARCHIVED">;
}

export type ProductPatch = Partial<Omit<ProductInput, "sku">>;

export const productsApi = {
  list: (q: ProductQuery = {}, signal?: AbortSignal) =>
    api.get<Page<Product>>("/api/v1/products", { query: { ...q }, signal }),
  get: (id: number, signal?: AbortSignal) => api.get<Product>(`/api/v1/products/${id}`, { signal }),
  create: (input: ProductInput) => api.post<Product>("/api/v1/products", input),
  update: (id: number, patch: ProductPatch) => api.patch<Product>(`/api/v1/products/${id}`, patch),
  archive: (id: number) => api.delete(`/api/v1/products/${id}`),
};

// --- inventory (admin) -------------------------------------------------------------

export const inventoryApi = {
  get: (productId: number, signal?: AbortSignal) =>
    api.get<Inventory>(`/api/v1/products/${productId}/inventory`, { signal }),
  /** Relative change: +restock / -damaged. */
  adjust: (productId: number, adjustment: number) =>
    api.patch<Inventory>(`/api/v1/products/${productId}/inventory`, { adjustment }),
  /** Absolute stock count; version = the version you looked at (optimistic locking). */
  set: (productId: number, availableQuantity: number, version: number) =>
    api.patch<Inventory>(`/api/v1/products/${productId}/inventory`, {
      available_quantity: availableQuantity,
      version,
    }),
};

// --- orders & payments ---------------------------------------------------------------

export interface OrderQuery {
  status?: OrderStatus;
  limit?: number;
  offset?: number;
}

export const ordersApi = {
  /**
   * Places an order. The Idempotency-Key makes retries safe: sending the same
   * key again returns the ORIGINAL order (200 + Idempotent-Replayed) instead
   * of creating a second one.
   */
  create: async (items: { product_id: number; quantity: number }[], idempotencyKey: string) => {
    const res = await request<Order>("POST", "/api/v1/orders", {
      body: { items },
      headers: { "Idempotency-Key": idempotencyKey },
    });
    return { order: res.data, replayed: res.headers.get("Idempotent-Replayed") === "true" };
  },
  list: (q: OrderQuery = {}, signal?: AbortSignal) =>
    api.get<Page<Order>>("/api/v1/orders", { query: { ...q }, signal }),
  get: (id: number, signal?: AbortSignal) => api.get<Order>(`/api/v1/orders/${id}`, { signal }),
  cancel: (id: number) => api.post<Order>(`/api/v1/orders/${id}/cancel`),
  /** simulate: only honoured by the backend's MOCK payment provider. */
  pay: (id: number, simulate?: SimulatedOutcome) =>
    api.post<PayResult>(`/api/v1/orders/${id}/pay`, simulate ? { simulate } : undefined),
};

// --- system ------------------------------------------------------------------------

export const systemApi = {
  /**
   * /ready answers 503 when a required dependency is down, but its body
   * still says WHICH one - so read the body whatever the status.
   */
  ready: async (): Promise<ReadyStatus> => {
    try {
      const res = await fetch(`${API_BASE_URL}/ready`);
      return (await res.json()) as ReadyStatus;
    } catch {
      return { status: "not_ready", checks: {} };
    }
  },
};
