import { useState } from "react";
import { Link } from "react-router";
import { useCart } from "../cart/CartContext";
import type { Product } from "../types/api";
import { formatMoney } from "../utils/format";

export function ProductCard({ product }: { product: Product }) {
  const { add } = useCart();
  const [message, setMessage] = useState<{ tone: "success" | "danger"; text: string } | null>(null);

  const onAdd = () => {
    const err = add(product, 1);
    setMessage(err ? { tone: "danger", text: err } : { tone: "success", text: "Added to cart" });
    setTimeout(() => setMessage(null), 2500);
  };

  return (
    <article className="card product-card">
      <Link to={`/products/${product.id}`} className="product-thumb" aria-hidden="true" tabIndex={-1}>
        {product.name.slice(0, 1).toUpperCase()}
      </Link>
      <div className="product-body">
        <Link to={`/products/${product.id}`} className="product-name">
          {product.name}
        </Link>
        <div className="muted small">SKU {product.sku}</div>
        {product.description && <p className="product-desc">{product.description}</p>}
      </div>
      <div className="product-footer">
        <span className="price">{formatMoney(product.price, product.currency)}</span>
        {product.status === "ACTIVE" ? (
          <button className="btn btn-primary btn-sm" onClick={onAdd}>
            Add to cart
          </button>
        ) : (
          <span className="muted small">Unavailable</span>
        )}
      </div>
      {message && <div className={`toast toast-${message.tone}`}>{message.text}</div>}
    </article>
  );
}
