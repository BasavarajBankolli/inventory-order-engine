import { useMemo, useState } from "react";
import { Link, Navigate, useNavigate } from "react-router";
import { ApiError } from "../api/client";
import { ordersApi } from "../api/services";
import { useAuth } from "../auth/AuthContext";
import { useCart } from "../cart/CartContext";
import { ErrorAlert, Notice, PageHeader } from "../components/ui";
import { formatMoney } from "../utils/format";

/**
 * Checkout = place the order. The backend reserves the stock in the same
 * transaction, so "Place order" either succeeds with the items held for
 * you, or fails with OUT_OF_STOCK and nothing is reserved. Payment happens
 * next, on the order page.
 */
export function CheckoutPage() {
  const { items, currency, subtotal, clear } = useCart();
  const { user } = useAuth();
  const navigate = useNavigate();
  const [error, setError] = useState<unknown>();
  const [submitting, setSubmitting] = useState(false);

  // One idempotency key per version of the cart. Clicking twice, or
  // retrying after a network error, re-sends the SAME key, and the backend
  // returns the order it already created instead of making a second one.
  // Changing the cart makes a new key (it is a different order).
  const cartSignature = items.map((i) => `${i.productId}:${i.quantity}`).join(",");
  const idempotencyKey = useMemo(() => crypto.randomUUID(), [cartSignature]); // eslint-disable-line react-hooks/exhaustive-deps

  if (items.length === 0 && !submitting) return <Navigate to="/cart" replace />;

  const placeOrder = async () => {
    setSubmitting(true);
    setError(undefined);
    try {
      const { order } = await ordersApi.create(
        items.map((i) => ({ product_id: i.productId, quantity: i.quantity })),
        idempotencyKey,
      );
      // The order (with its reserved stock) is now the source of truth.
      clear();
      navigate(`/orders/${order.id}?placed=1`, { replace: true });
    } catch (err) {
      setError(err);
      setSubmitting(false);
    }
  };

  const outOfStock = error instanceof ApiError && error.code === "OUT_OF_STOCK";

  return (
    <>
      <PageHeader title="Checkout" subtitle={`Ordering as ${user?.name} (${user?.email})`} />

      <div className="cart-layout">
        <div className="card">
          <h2>Order summary</h2>
          <table className="table">
            <thead>
              <tr>
                <th>Product</th>
                <th className="num">Qty</th>
                <th className="num">Price</th>
                <th className="num">Total</th>
              </tr>
            </thead>
            <tbody>
              {items.map((i) => (
                <tr key={i.productId}>
                  <td>
                    {i.name}
                    <div className="muted small">SKU {i.sku}</div>
                  </td>
                  <td className="num">{i.quantity}</td>
                  <td className="num">{formatMoney(i.price, i.currency)}</td>
                  <td className="num">{formatMoney(i.price * i.quantity, i.currency)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>

        <aside className="card summary">
          <h2>Total</h2>
          <div className="row-between total-row">
            <span>To pay</span>
            <strong>{currency && formatMoney(subtotal, currency)}</strong>
          </div>

          {outOfStock ? (
            <Notice tone="danger">
              <strong>Not enough stock.</strong> {(error as ApiError).message}. Please{" "}
              <Link to="/cart">adjust your cart</Link> and try again.
            </Notice>
          ) : (
            error !== undefined && <ErrorAlert error={error} />
          )}

          <button className="btn btn-primary btn-block" disabled={submitting} onClick={placeOrder}>
            {submitting ? "Placing order…" : "Place order"}
          </button>
          <p className="muted small">
            Your items are reserved as soon as the order is placed. You pay on the next step.
          </p>
          <Link to="/cart" className="btn btn-ghost btn-block">
            Back to cart
          </Link>
        </aside>
      </div>
    </>
  );
}
