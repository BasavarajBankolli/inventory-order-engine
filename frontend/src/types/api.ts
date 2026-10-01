// TypeScript mirrors of the backend's JSON (see the Go structs' json tags).
// All money amounts are integers in MINOR units (paise/cents).

export type Role = "CUSTOMER" | "ADMIN";

export interface User {
  id: number;
  email: string;
  name: string;
  role: Role;
  created_at: string;
}

export interface LoginResponse {
  access_token: string;
  token_type: "Bearer";
  expires_in: number;
  expires_at: string;
  user: User;
}

export type ProductStatus = "ACTIVE" | "INACTIVE" | "ARCHIVED";

export interface Product {
  id: number;
  sku: string;
  name: string;
  description: string;
  price: number; // minor units
  currency: string;
  status: ProductStatus;
  created_at: string;
  updated_at: string;
}

/** Paginated list response: GET /products, GET /orders. */
export interface Page<T> {
  items: T[];
  total: number;
  limit: number;
  offset: number;
}

export type ProductSort = "newest" | "oldest" | "price_asc" | "price_desc" | "name";

export interface Inventory {
  product_id: number;
  available_quantity: number;
  reserved_quantity: number;
  version: number;
  updated_at: string;
}

export type OrderStatus =
  | "CREATED"
  | "RESERVED"
  | "PAYMENT_PENDING"
  | "PAYMENT_FAILED"
  | "CONFIRMED"
  | "PROCESSING"
  | "SHIPPED"
  | "DELIVERED"
  | "CANCELLED"
  | "EXPIRED";

export interface OrderItem {
  product_id: number;
  quantity: number;
  unit_price: number;
  total_price: number;
}

export interface Order {
  id: number;
  user_id: number;
  status: OrderStatus;
  total_amount: number;
  currency: string;
  items?: OrderItem[]; // omitted in list responses
  created_at: string;
  updated_at: string;
}

export type PaymentStatus = "PENDING" | "SUCCEEDED" | "FAILED";

export interface Payment {
  id: number;
  order_id: number;
  amount: number;
  currency: string;
  status: PaymentStatus;
  provider_reference?: string;
  failure_reason?: string;
  created_at: string;
  updated_at: string;
}

export interface PayResult {
  order: Order;
  payment: Payment;
}

/** Mock payment provider outcomes the backend accepts in {"simulate": ...}. */
export type SimulatedOutcome = "SUCCESS" | "FAILURE" | "TIMEOUT";

/** The backend's error body: {"error": {...}}. */
export interface ApiErrorBody {
  error: {
    code: string;
    message: string;
    fields?: Record<string, string>;
    request_id?: string;
  };
}

export interface ReadyStatus {
  status: "ready" | "degraded" | "not_ready";
  checks: Record<string, string>;
}
