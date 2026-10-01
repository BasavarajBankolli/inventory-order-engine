import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import type { Product } from "../types/api";

// The backend has no cart API (orders are created in one request), so the
// cart lives in the browser (localStorage). Prices shown here are a
// SNAPSHOT for display; the backend always charges its current price, and
// the order it returns shows what was actually charged.

const STORAGE_KEY = "stockflow.cart";

// Limits enforced by the backend (orders.validateItems).
export const MAX_LINES = 50;
export const MAX_QUANTITY = 1000;

export interface CartItem {
  productId: number;
  sku: string;
  name: string;
  price: number; // minor units, snapshot
  currency: string;
  quantity: number;
}

interface CartContextValue {
  items: CartItem[];
  count: number; // total units
  currency: string | null; // one currency per order (backend rule)
  subtotal: number;
  /** Returns an error message instead of adding, or null on success. */
  add: (product: Product, quantity?: number) => string | null;
  setQuantity: (productId: number, quantity: number) => void;
  remove: (productId: number) => void;
  /** Refresh snapshot data (e.g. a changed price) from the server. */
  refresh: (product: Product) => void;
  clear: () => void;
}

const CartContext = createContext<CartContextValue | null>(null);

function load(): CartItem[] {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    return raw ? (JSON.parse(raw) as CartItem[]) : [];
  } catch {
    return [];
  }
}

const clamp = (n: number) => Math.min(Math.max(Math.floor(n) || 1, 1), MAX_QUANTITY);

export function CartProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<CartItem[]>(load);

  useEffect(() => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(items));
  }, [items]);

  const currency = items[0]?.currency ?? null;

  const add = useCallback(
    (product: Product, quantity = 1): string | null => {
      if (product.status !== "ACTIVE") return "This product is not available right now.";
      if (currency && product.currency !== currency) {
        return `Your cart is in ${currency}. Products in another currency must be ordered separately.`;
      }
      const existing = items.find((i) => i.productId === product.id);
      if (!existing && items.length >= MAX_LINES) return `A cart can hold at most ${MAX_LINES} different products.`;

      setItems((prev) => {
        const found = prev.find((i) => i.productId === product.id);
        if (found) {
          return prev.map((i) => (i.productId === product.id ? { ...i, quantity: clamp(i.quantity + quantity) } : i));
        }
        return [
          ...prev,
          {
            productId: product.id,
            sku: product.sku,
            name: product.name,
            price: product.price,
            currency: product.currency,
            quantity: clamp(quantity),
          },
        ];
      });
      return null;
    },
    [items, currency],
  );

  const setQuantity = useCallback((productId: number, quantity: number) => {
    setItems((prev) => prev.map((i) => (i.productId === productId ? { ...i, quantity: clamp(quantity) } : i)));
  }, []);

  const remove = useCallback((productId: number) => {
    setItems((prev) => prev.filter((i) => i.productId !== productId));
  }, []);

  const refresh = useCallback((p: Product) => {
    setItems((prev) =>
      prev.map((i) => (i.productId === p.id ? { ...i, name: p.name, price: p.price, currency: p.currency } : i)),
    );
  }, []);

  const clear = useCallback(() => setItems([]), []);

  const value = useMemo<CartContextValue>(
    () => ({
      items,
      count: items.reduce((n, i) => n + i.quantity, 0),
      currency,
      subtotal: items.reduce((sum, i) => sum + i.price * i.quantity, 0),
      add,
      setQuantity,
      remove,
      refresh,
      clear,
    }),
    [items, currency, add, setQuantity, remove, refresh, clear],
  );

  return <CartContext.Provider value={value}>{children}</CartContext.Provider>;
}

export function useCart(): CartContextValue {
  const ctx = useContext(CartContext);
  if (!ctx) throw new Error("useCart must be used inside <CartProvider>");
  return ctx;
}
