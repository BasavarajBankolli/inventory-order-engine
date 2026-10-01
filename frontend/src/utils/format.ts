import type { OrderStatus } from "../types/api";

// --- money -------------------------------------------------------------------------
// The backend stores integers in MINOR units (paise, cents). How many
// decimals a currency has comes from Intl (INR/USD: 2, JPY: 0).

function fractionDigits(currency: string): number {
  try {
    return new Intl.NumberFormat("en", { style: "currency", currency }).resolvedOptions().maximumFractionDigits ?? 2;
  } catch {
    return 2;
  }
}

/** 129900, "INR" -> "₹1,299.00" */
export function formatMoney(minor: number, currency: string): string {
  const digits = fractionDigits(currency);
  const major = minor / 10 ** digits;
  try {
    return new Intl.NumberFormat(undefined, { style: "currency", currency }).format(major);
  } catch {
    return `${major.toFixed(digits)} ${currency}`;
  }
}

/** "1299.50", "INR" -> 129950. Returns NaN for invalid input. */
export function toMinorUnits(major: string, currency: string): number {
  const value = Number(major);
  if (major.trim() === "" || !Number.isFinite(value)) return NaN;
  return Math.round(value * 10 ** fractionDigits(currency));
}

/** 129950, "INR" -> "1299.50" (for form inputs). */
export function toMajorString(minor: number, currency: string): string {
  const digits = fractionDigits(currency);
  return (minor / 10 ** digits).toFixed(digits);
}

// --- dates ---------------------------------------------------------------------------

export function formatDate(iso: string): string {
  return new Date(iso).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
}

// --- order status --------------------------------------------------------------------

export const ORDER_STATUS_LABEL: Record<OrderStatus, string> = {
  CREATED: "Created",
  RESERVED: "Awaiting payment",
  PAYMENT_PENDING: "Payment pending",
  PAYMENT_FAILED: "Payment failed",
  CONFIRMED: "Confirmed",
  PROCESSING: "Processing",
  SHIPPED: "Shipped",
  DELIVERED: "Delivered",
  CANCELLED: "Cancelled",
  EXPIRED: "Expired",
};

/** What the customer should know about each status. */
export const ORDER_STATUS_HELP: Record<OrderStatus, string> = {
  CREATED: "Your order was created.",
  RESERVED: "Your items are reserved for you. Complete the payment before the reservation expires.",
  PAYMENT_PENDING:
    "We have not heard back from the payment provider yet. You can safely retry - you will never be charged twice.",
  PAYMENT_FAILED: "The payment did not go through.",
  CONFIRMED: "Payment received - your order is confirmed.",
  PROCESSING: "Your order is being prepared.",
  SHIPPED: "Your order is on its way.",
  DELIVERED: "Your order was delivered.",
  CANCELLED: "This order was cancelled and the items were released.",
  EXPIRED: "The reservation expired before payment, so the items were released.",
};

/** Colour group for status badges. */
export function statusTone(status: string): "success" | "warning" | "danger" | "info" | "neutral" {
  switch (status) {
    case "CONFIRMED":
    case "DELIVERED":
    case "SHIPPED":
    case "ACTIVE":
    case "SUCCEEDED":
      return "success";
    case "RESERVED":
    case "PAYMENT_PENDING":
    case "PROCESSING":
    case "PENDING":
      return "warning";
    case "PAYMENT_FAILED":
    case "CANCELLED":
    case "FAILED":
      return "danger";
    case "CREATED":
      return "info";
    default:
      return "neutral"; // EXPIRED, INACTIVE, ...
  }
}
