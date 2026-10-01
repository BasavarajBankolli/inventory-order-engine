import { useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router";
import { ApiError } from "../api/client";
import { ordersApi, productsApi } from "../api/services";
import { useAuth } from "../auth/AuthContext";
import { useCart } from "../cart/CartContext";
import { BackLink, EmptyState, ErrorAlert, Notice, Spinner, StatusBadge } from "../components/ui";
import { useAsync } from "../hooks/useAsync";
import type { Order, Payment, Product, SimulatedOutcome } from "../types/api";
import { ORDER_STATUS_HELP, ORDER_STATUS_LABEL, formatDate, formatMoney } from "../utils/format";

const PAYABLE = ["RESERVED", "PAYMENT_PENDING"];
const CANCELLABLE = ["CREATED", "RESERVED", "PAYMENT_FAILED"];
const REORDERABLE = ["CANCELLED", "EXPIRED"];

/** Loads an order plus the products it contains (order items only carry ids). */
async function loadOrder(id: number, signal: AbortSignal) {
  const order = await ordersApi.get(id, signal);
  const products = new Map<number, Product | null>();
  await Promise.all(
    (order.items ?? []).map(async (item) => {
      // A product archived after the order returns 404; the order line
      // still shows its snapshot price.
      const p = await productsApi.get(item.product_id, signal).catch(() => null);
      products.set(item.product_id, p);
    }),
  );
  return { order, products };
}

type Feedback = { tone: "success" | "warning" | "danger"; text: string } | null;

export function OrderDetailsPage() {
  const id = Number(useParams().id);
  const [params] = useSearchParams();
  const { isAdmin } = useAuth();
  const { add } = useCart();
  const navigate = useNavigate();

  const { data, error, loading, reload } = useAsync((signal) => loadOrder(id, signal), [id]);
  const [busy, setBusy] = useState<"pay" | "cancel" | "reorder" | null>(null);
  const [feedback, setFeedback] = useState<Feedback>(
    params.get("placed") ? { tone: "success", text: "Order placed! Your items are reserved. Complete the payment below." } : null,
  );
  const [payment, setPayment] = useState<Payment | null>(null);
  const [outcome, setOutcome] = useState<SimulatedOutcome>("SUCCESS");

  if (loading && !data) return <Spinner label="Loading order…" />;
  if (error instanceof ApiError && (error.status === 404 || error.status === 400)) {
    // The backend answers 404 (not 403) for other people's orders.
    return <EmptyState title="Order not found" action={<Link to="/orders" className="btn">Back to orders</Link>} />;
  }
  if (error !== undefined || !data) return <ErrorAlert error={error} onRetry={reload} />;

  const { order, products } = data;

  const pay = async () => {
    setBusy("pay");
    setFeedback(null);
    try {
      const res = await ordersApi.pay(order.id, outcome);
      setPayment(res.payment);
      setFeedback({ tone: "success", text: "Payment successful - your order is confirmed." });
    } catch (err) {
      setFeedback(paymentFeedback(err));
    } finally {
      setBusy(null);
      reload(); // the order status changed in every case
    }
  };

  const cancel = async () => {
    if (!window.confirm(`Cancel order #${order.id}? Reserved items will be released.`)) return;
    setBusy("cancel");
    setFeedback(null);
    try {
      await ordersApi.cancel(order.id);
      setFeedback({ tone: "success", text: "Order cancelled." });
      reload();
    } catch (err) {
      setFeedback({ tone: "danger", text: err instanceof Error ? err.message : "Could not cancel the order." });
    } finally {
      setBusy(null);
    }
  };

  // Put the items of a cancelled/expired order back into the cart.
  const reorder = () => {
    setBusy("reorder");
    const problems: string[] = [];
    for (const item of order.items ?? []) {
      const p = products.get(item.product_id);
      const err = p ? add(p, item.quantity) : "no longer available";
      if (err) problems.push(`${p?.name ?? `Product #${item.product_id}`}: ${err}`);
    }
    setBusy(null);
    if (problems.length) setFeedback({ tone: "warning", text: `Some items could not be added - ${problems.join("; ")}` });
    else navigate("/cart");
  };

  return (
    <>
      <BackLink to="/orders">{isAdmin ? "All orders" : "My orders"}</BackLink>

      <div className="page-header">
        <div>
          <h1>Order #{order.id}</h1>
          <p className="muted">
            Placed {formatDate(order.created_at)}
            {isAdmin && ` · Customer user #${order.user_id}`}
          </p>
        </div>
        <StatusBadge status={order.status} label={ORDER_STATUS_LABEL[order.status]} />
      </div>

      {feedback && <Notice tone={feedback.tone}>{feedback.text}</Notice>}

      <div className="cart-layout">
        <div className="card">
          <h2>Items</h2>
          <OrderItemsTable order={order} products={products} />
        </div>

        <aside className="card summary">
          <h2>Status</h2>
          <p>{ORDER_STATUS_HELP[order.status]}</p>

          {payment && (
            <p className="muted small">
              Payment reference: <code>{payment.provider_reference}</code>
            </p>
          )}

          {PAYABLE.includes(order.status) && (
            <div className="pay-box">
              <label className="field">
                <span className="field-label">Payment simulation (demo)</span>
                <select value={outcome} onChange={(e) => setOutcome(e.target.value as SimulatedOutcome)}>
                  <option value="SUCCESS">Card approved</option>
                  <option value="FAILURE">Card declined</option>
                  <option value="TIMEOUT">Provider times out</option>
                </select>
                <span className="field-hint">The backend uses a mock payment provider; pick what it should do.</span>
              </label>
              <button className="btn btn-primary btn-block" onClick={pay} disabled={busy !== null}>
                {busy === "pay"
                  ? "Processing payment…"
                  : order.status === "PAYMENT_PENDING"
                    ? "Retry payment"
                    : `Pay ${formatMoney(order.total_amount, order.currency)}`}
              </button>
            </div>
          )}

          {CANCELLABLE.includes(order.status) && (
            <button className="btn btn-block btn-danger-outline" onClick={cancel} disabled={busy !== null}>
              {busy === "cancel" ? "Cancelling…" : "Cancel order"}
            </button>
          )}

          {REORDERABLE.includes(order.status) && !isAdmin && (
            <button className="btn btn-block" onClick={reorder} disabled={busy !== null}>
              Put items back in cart
            </button>
          )}
        </aside>
      </div>
    </>
  );
}

function OrderItemsTable({ order, products }: { order: Order; products: Map<number, Product | null> }) {
  return (
    <table className="table">
      <thead>
        <tr>
          <th>Product</th>
          <th className="num">Qty</th>
          <th className="num">Unit price</th>
          <th className="num">Total</th>
        </tr>
      </thead>
      <tbody>
        {(order.items ?? []).map((item) => {
          const p = products.get(item.product_id);
          return (
            <tr key={item.product_id}>
              <td>
                {p ? <Link to={`/products/${p.id}`}>{p.name}</Link> : `Product #${item.product_id}`}
                {p ? <div className="muted small">SKU {p.sku}</div> : <div className="muted small">No longer in the catalogue</div>}
              </td>
              <td className="num">{item.quantity}</td>
              <td className="num">{formatMoney(item.unit_price, order.currency)}</td>
              <td className="num">{formatMoney(item.total_price, order.currency)}</td>
            </tr>
          );
        })}
      </tbody>
      <tfoot>
        <tr>
          <td colSpan={3}>
            <strong>Total</strong>
          </td>
          <td className="num">
            <strong>{formatMoney(order.total_amount, order.currency)}</strong>
          </td>
        </tr>
      </tfoot>
    </table>
  );
}

/** Turns the backend's payment errors into what the customer should do next. */
function paymentFeedback(err: unknown): Feedback {
  if (err instanceof ApiError) {
    switch (err.code) {
      case "PAYMENT_FAILED":
        return { tone: "danger", text: "Your payment was declined. The order was cancelled and the items released - you can put them back in your cart and try again." };
      case "PAYMENT_TIMEOUT":
        return { tone: "warning", text: "The payment provider did not answer in time. Your order is still reserved - retry the payment. You will never be charged twice." };
      case "RESERVATION_EXPIRED":
        return { tone: "warning", text: "Your reservation expired before payment. If you were charged, the order will be confirmed automatically; otherwise please order again." };
      case "INVALID_STATE_TRANSITION":
        return { tone: "warning", text: "This order can no longer be paid (its status changed). The page has been refreshed." };
    }
  }
  return { tone: "danger", text: err instanceof Error ? err.message : "Payment failed." };
}
