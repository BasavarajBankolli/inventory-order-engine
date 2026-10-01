import { useState } from "react";
import { useSearchParams } from "react-router";
import { ApiError } from "../../api/client";
import { inventoryApi, productsApi } from "../../api/services";
import { AdminNav } from "../../components/Layout";
import { EmptyState, ErrorAlert, PageHeader, Pagination, Spinner, StatusBadge } from "../../components/ui";
import { useAsync, useDebounced } from "../../hooks/useAsync";
import type { Inventory, Product } from "../../types/api";

// Inventory is per product (GET /products/{id}/inventory), so a page loads
// its products, then each product's stock. A small page keeps that cheap.
const PAGE_SIZE = 10;
const LOW_STOCK = 5;

async function loadPage(q: string, status: "ACTIVE" | "INACTIVE", offset: number, signal: AbortSignal) {
  const page = await productsApi.list({ q, status, sort: "name", limit: PAGE_SIZE, offset }, signal);
  const rows = await Promise.all(
    page.items.map(async (product) => ({ product, inventory: await inventoryApi.get(product.id, signal) })),
  );
  return { ...page, rows };
}

export function AdminInventoryPage() {
  const [params, setParams] = useSearchParams();
  const status = params.get("status") === "INACTIVE" ? "INACTIVE" : "ACTIVE";
  const offset = Number(params.get("offset")) || 0;
  const [search, setSearch] = useState(params.get("q") ?? "");
  const q = useDebounced(search.trim());

  const { data, error, loading, reload } = useAsync((signal) => loadPage(q, status, offset, signal), [q, status, offset]);

  const setParam = (key: string, value: string | null) => {
    const next = new URLSearchParams(params);
    if (value) next.set(key, value);
    else next.delete(key);
    if (key !== "offset") next.delete("offset");
    setParams(next, { replace: true });
  };

  return (
    <>
      <PageHeader
        title="Inventory"
        subtitle="Available = can be sold now. Reserved = held for unpaid orders (released automatically if they expire)."
      />
      <AdminNav />

      <div className="toolbar">
        <input
          type="search"
          className="search"
          placeholder="Search by name or SKU…"
          value={search}
          onChange={(e) => {
            setSearch(e.target.value);
            setParam("offset", null);
          }}
        />
        <select value={status} onChange={(e) => setParam("status", e.target.value === "ACTIVE" ? null : e.target.value)}>
          <option value="ACTIVE">Active products</option>
          <option value="INACTIVE">Inactive products</option>
        </select>
      </div>

      {error !== undefined ? (
        <ErrorAlert error={error} onRetry={reload} />
      ) : loading && !data ? (
        <Spinner label="Loading stock levels…" />
      ) : data && data.rows.length === 0 ? (
        <EmptyState title="No products found" />
      ) : (
        data && (
          <>
            <div className={`card table-card ${loading ? "is-loading" : ""}`}>
              <table className="table inventory-table">
                <thead>
                  <tr>
                    <th>Product</th>
                    <th className="num">Available</th>
                    <th className="num">Reserved</th>
                    <th>Update stock</th>
                  </tr>
                </thead>
                <tbody>
                  {data.rows.map((r) => (
                    <InventoryRow key={r.product.id} product={r.product} initial={r.inventory} />
                  ))}
                </tbody>
              </table>
            </div>
            <Pagination total={data.total} limit={data.limit} offset={data.offset} onChange={(o) => setParam("offset", String(o))} />
          </>
        )
      )}
    </>
  );
}

function InventoryRow({ product, initial }: { product: Product; initial: Inventory }) {
  const [inv, setInv] = useState(initial);
  const [adjust, setAdjust] = useState("");
  const [count, setCount] = useState("");
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<{ tone: "success" | "danger"; text: string } | null>(null);

  const run = async (action: () => Promise<Inventory>, success: string) => {
    setBusy(true);
    setMessage(null);
    try {
      const updated = await action();
      setInv(updated);
      setAdjust("");
      setCount("");
      setMessage({ tone: "success", text: success });
    } catch (err) {
      if (err instanceof ApiError && err.code === "VERSION_CONFLICT") {
        // Someone (an order, another admin) changed the stock since we
        // loaded it. Show the fresh numbers and let the admin decide again.
        setInv(await inventoryApi.get(product.id));
        setMessage({ tone: "danger", text: "Stock changed meanwhile - reloaded the current numbers. Please try again." });
      } else if (err instanceof ApiError && Object.keys(err.fields).length) {
        setMessage({ tone: "danger", text: Object.values(err.fields).join(" ") });
      } else {
        setMessage({ tone: "danger", text: err instanceof Error ? err.message : "Update failed." });
      }
    } finally {
      setBusy(false);
    }
  };

  const delta = Number(adjust);
  const target = Number(count);

  return (
    <tr>
      <td>
        {product.name}
        <div className="muted small">SKU {product.sku}</div>
      </td>
      <td className="num">
        <strong>{inv.available_quantity}</strong>{" "}
        {inv.available_quantity === 0 ? (
          <StatusBadge status="FAILED" label="Out" />
        ) : (
          inv.available_quantity <= LOW_STOCK && <StatusBadge status="PENDING" label="Low" />
        )}
      </td>
      <td className="num">{inv.reserved_quantity}</td>
      <td>
        <div className="stock-actions">
          <form
            className="inline-form"
            onSubmit={(e) => {
              e.preventDefault();
              if (delta) run(() => inventoryApi.adjust(product.id, delta), `${delta > 0 ? "Added" : "Removed"} ${Math.abs(delta)}.`);
            }}
          >
            <input
              type="number"
              placeholder="+10 / -2"
              value={adjust}
              onChange={(e) => setAdjust(e.target.value)}
              aria-label={`Adjust stock of ${product.name}`}
            />
            <button className="btn btn-sm" disabled={busy || !delta}>
              Adjust
            </button>
          </form>
          <form
            className="inline-form"
            onSubmit={(e) => {
              e.preventDefault();
              if (count !== "") run(() => inventoryApi.set(product.id, target, inv.version), `Stock set to ${target}.`);
            }}
          >
            <input
              type="number"
              min={0}
              placeholder="Count"
              value={count}
              onChange={(e) => setCount(e.target.value)}
              aria-label={`Set exact stock of ${product.name}`}
            />
            <button className="btn btn-sm" disabled={busy || count === ""}>
              Set
            </button>
          </form>
        </div>
        {message && <div className={`row-msg ${message.tone === "danger" ? "field-error" : "text-success"}`}>{message.text}</div>}
      </td>
    </tr>
  );
}
