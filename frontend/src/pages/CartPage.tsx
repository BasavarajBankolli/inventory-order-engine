import { useEffect, useState } from "react";
import { Link, useNavigate } from "react-router";
import { ApiError } from "../api/client";
import { productsApi } from "../api/services";
import { useAuth } from "../auth/AuthContext";
import { MAX_QUANTITY, useCart } from "../cart/CartContext";
import { EmptyState, Notice, PageHeader } from "../components/ui";
import { formatMoney } from "../utils/format";

export function CartPage() {
  const { items, currency, subtotal, setQuantity, remove, refresh } = useCart();
  const { user } = useAuth();
  const navigate = useNavigate();
  const [unavailable, setUnavailable] = useState<Set<number>>(new Set());
  const [priceChanged, setPriceChanged] = useState(false);

  // Cart prices are a snapshot from when the item was added. Re-check each
  // product against the backend: update changed prices, flag products that
  // were archived or deactivated since.
  const ids = items.map((i) => i.productId).join(",");
  useEffect(() => {
    const controller = new AbortController();
    const snapshot = new Map(items.map((i) => [i.productId, i.price]));
    Promise.all(
      items.map((item) =>
        productsApi.get(item.productId, controller.signal).then(
          (p) => ({ id: item.productId, product: p }),
          (err: unknown) => ({ id: item.productId, product: null, gone: err instanceof ApiError && err.status === 404 }),
        ),
      ),
    ).then((results) => {
      if (controller.signal.aborted) return;
      const bad = new Set<number>();
      let changed = false;
      for (const r of results) {
        if (!r.product) {
          if ("gone" in r && r.gone) bad.add(r.id);
          continue;
        }
        if (r.product.status !== "ACTIVE") bad.add(r.id);
        if (r.product.price !== snapshot.get(r.id)) changed = true;
        refresh(r.product);
      }
      setUnavailable(bad);
      setPriceChanged(changed);
    });
    return () => controller.abort();
    // re-check only when the SET of products changes, not on quantity edits
  }, [ids]); // eslint-disable-line react-hooks/exhaustive-deps

  if (items.length === 0) {
    return (
      <>
        <PageHeader title="Your cart" />
        <EmptyState
          title="Your cart is empty"
          text="Find something you like in the shop."
          action={<Link to="/products" className="btn btn-primary">Browse products</Link>}
        />
      </>
    );
  }

  const blocked = items.some((i) => unavailable.has(i.productId));

  return (
    <>
      <PageHeader title="Your cart" subtitle={`${items.length} product${items.length > 1 ? "s" : ""}`} />
      {priceChanged && <Notice tone="warning">Some prices changed since you added the items. Your cart shows the current prices.</Notice>}
      {blocked && <Notice tone="danger">Some products are no longer available. Remove them to continue.</Notice>}

      <div className="cart-layout">
        <div className="card cart-items">
          {items.map((item) => (
            <div key={item.productId} className={`cart-row ${unavailable.has(item.productId) ? "is-unavailable" : ""}`}>
              <div className="cart-thumb" aria-hidden="true">
                {item.name.slice(0, 1).toUpperCase()}
              </div>
              <div className="cart-info">
                <Link to={`/products/${item.productId}`} className="product-name">
                  {item.name}
                </Link>
                <div className="muted small">
                  SKU {item.sku} · {formatMoney(item.price, item.currency)} each
                </div>
                {unavailable.has(item.productId) && <div className="field-error">No longer available</div>}
              </div>
              <input
                className="qty-input"
                type="number"
                min={1}
                max={MAX_QUANTITY}
                value={item.quantity}
                aria-label={`Quantity of ${item.name}`}
                onChange={(e) => setQuantity(item.productId, Number(e.target.value))}
              />
              <div className="cart-line-total">{formatMoney(item.price * item.quantity, item.currency)}</div>
              <button className="btn btn-sm btn-ghost" onClick={() => remove(item.productId)} aria-label={`Remove ${item.name}`}>
                Remove
              </button>
            </div>
          ))}
        </div>

        <aside className="card summary">
          <h2>Summary</h2>
          <div className="row-between">
            <span>Subtotal</span>
            <strong>{currency && formatMoney(subtotal, currency)}</strong>
          </div>
          <p className="muted small">The final price is confirmed when you place the order.</p>
          <button
            className="btn btn-primary btn-block"
            disabled={blocked}
            onClick={() => navigate(user ? "/checkout" : "/login?next=%2Fcheckout")}
          >
            {user ? "Checkout" : "Log in to checkout"}
          </button>
          <Link to="/products" className="btn btn-ghost btn-block">
            Continue shopping
          </Link>
        </aside>
      </div>
    </>
  );
}
