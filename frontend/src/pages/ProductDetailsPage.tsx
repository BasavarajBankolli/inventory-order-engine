import { useState } from "react";
import { Link, useParams } from "react-router";
import { ApiError } from "../api/client";
import { productsApi } from "../api/services";
import { useAuth } from "../auth/AuthContext";
import { MAX_QUANTITY, useCart } from "../cart/CartContext";
import { BackLink, EmptyState, ErrorAlert, Notice, Spinner, StatusBadge } from "../components/ui";
import { useAsync } from "../hooks/useAsync";
import { formatMoney } from "../utils/format";

export function ProductDetailsPage() {
  const id = Number(useParams().id);
  const { isAdmin } = useAuth();
  const { add } = useCart();
  const [quantity, setQuantity] = useState(1);
  const [message, setMessage] = useState<{ tone: "success" | "danger"; text: string } | null>(null);

  const { data: product, error, loading, reload } = useAsync((signal) => productsApi.get(id, signal), [id]);

  if (loading) return <Spinner />;
  if (error instanceof ApiError && (error.status === 404 || error.status === 400)) {
    return (
      <EmptyState
        title="Product not found"
        text="It may have been removed from the catalogue."
        action={<Link to="/products" className="btn">Back to shop</Link>}
      />
    );
  }
  if (error !== undefined || !product) return <ErrorAlert error={error} onRetry={reload} />;

  const onAdd = () => {
    const err = add(product, quantity);
    setMessage(err ? { tone: "danger", text: err } : { tone: "success", text: `Added ${quantity} × ${product.name} to your cart.` });
  };

  return (
    <>
      <BackLink to="/products">Back to shop</BackLink>
      <div className="details">
        <div className="details-media" aria-hidden="true">
          {product.name.slice(0, 1).toUpperCase()}
        </div>
        <div className="details-info card">
          <div className="row-between">
            <span className="muted small">SKU {product.sku}</span>
            {product.status !== "ACTIVE" && <StatusBadge status={product.status} />}
          </div>
          <h1>{product.name}</h1>
          <div className="price price-lg">{formatMoney(product.price, product.currency)}</div>
          {product.description ? <p>{product.description}</p> : <p className="muted">No description.</p>}

          {message && (
            <Notice tone={message.tone}>
              {message.text} {message.tone === "success" && <Link to="/cart">View cart →</Link>}
            </Notice>
          )}

          {product.status === "ACTIVE" ? (
            <div className="buy-row">
              <label className="qty">
                <span className="muted small">Quantity</span>
                <input
                  type="number"
                  min={1}
                  max={MAX_QUANTITY}
                  value={quantity}
                  onChange={(e) => setQuantity(Math.max(1, Math.min(MAX_QUANTITY, Number(e.target.value) || 1)))}
                />
              </label>
              <button className="btn btn-primary" onClick={onAdd}>
                Add to cart
              </button>
            </div>
          ) : (
            <Notice tone="warning">This product is currently not available for purchase.</Notice>
          )}
          <p className="muted small">Stock is checked and reserved for you when you place the order.</p>

          {isAdmin && (
            <div className="admin-hint">
              <Link to={`/admin/products?edit=${product.id}`}>Edit product</Link> ·{" "}
              <Link to={`/admin/inventory?q=${encodeURIComponent(product.sku)}`}>Manage stock</Link>
            </div>
          )}
        </div>
      </div>
    </>
  );
}
