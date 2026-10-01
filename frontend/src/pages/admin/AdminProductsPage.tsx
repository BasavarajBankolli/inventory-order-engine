import { useEffect, useState, type FormEvent } from "react";
import { useSearchParams } from "react-router";
import { ApiError } from "../../api/client";
import { productsApi, type ProductInput, type ProductPatch } from "../../api/services";
import { AdminNav } from "../../components/Layout";
import { EmptyState, ErrorAlert, Field, Notice, PageHeader, Pagination, Spinner, StatusBadge } from "../../components/ui";
import { useAsync, useDebounced } from "../../hooks/useAsync";
import type { Product } from "../../types/api";
import { formatDate, formatMoney, toMajorString, toMinorUnits } from "../../utils/format";

const PAGE_SIZE = 15;
type Editing = { mode: "create" } | { mode: "edit"; product: Product } | null;

export function AdminProductsPage() {
  const [params, setParams] = useSearchParams();
  const status = params.get("status") === "INACTIVE" ? "INACTIVE" : "ACTIVE";
  const offset = Number(params.get("offset")) || 0;
  const [search, setSearch] = useState("");
  const q = useDebounced(search.trim());
  const [editing, setEditing] = useState<Editing>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const { data, error, loading, reload } = useAsync(
    (signal) => productsApi.list({ q, status, sort: "newest", limit: PAGE_SIZE, offset }, signal),
    [q, status, offset],
  );

  // Deep link from a product page: /admin/products?edit=12
  const editId = Number(params.get("edit")) || 0;
  useEffect(() => {
    if (!editId) return;
    productsApi.get(editId).then((p) => setEditing({ mode: "edit", product: p }), () => undefined);
  }, [editId]);

  const setParam = (key: string, value: string | null) => {
    const next = new URLSearchParams(params);
    if (value) next.set(key, value);
    else next.delete(key);
    if (key !== "offset") next.delete("offset");
    next.delete("edit");
    setParams(next, { replace: true });
  };

  const onSaved = (p: Product, created: boolean) => {
    setEditing(null);
    setNotice(`${created ? "Created" : "Saved"} “${p.name}”.`);
    reload();
  };

  const archive = async (p: Product) => {
    if (!window.confirm(`Archive “${p.name}”? It disappears from the shop; existing orders keep it.`)) return;
    try {
      await productsApi.archive(p.id);
      setNotice(`Archived “${p.name}”.`);
      reload();
    } catch (err) {
      setNotice(err instanceof Error ? err.message : "Could not archive the product.");
    }
  };

  return (
    <>
      <PageHeader
        title="Products"
        subtitle="Create, edit and archive catalogue products. Stock is managed under Inventory."
        actions={
          <button className="btn btn-primary" onClick={() => setEditing({ mode: "create" })}>
            New product
          </button>
        }
      />
      <AdminNav />

      {notice && <Notice tone="success">{notice}</Notice>}
      {editing && (
        <ProductForm
          key={editing.mode === "edit" ? editing.product.id : "new"}
          product={editing.mode === "edit" ? editing.product : null}
          onCancel={() => setEditing(null)}
          onSaved={onSaved}
        />
      )}

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
          <option value="ACTIVE">Active</option>
          <option value="INACTIVE">Inactive</option>
        </select>
      </div>

      {error !== undefined ? (
        <ErrorAlert error={error} onRetry={reload} />
      ) : loading && !data ? (
        <Spinner />
      ) : data && data.items.length === 0 ? (
        <EmptyState title="No products found" />
      ) : (
        data && (
          <>
            <div className={`card table-card ${loading ? "is-loading" : ""}`}>
              <table className="table">
                <thead>
                  <tr>
                    <th>Product</th>
                    <th className="num">Price</th>
                    <th>Status</th>
                    <th className="hide-sm">Updated</th>
                    <th className="num">Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {data.items.map((p) => (
                    <tr key={p.id}>
                      <td>
                        {p.name}
                        <div className="muted small">SKU {p.sku}</div>
                      </td>
                      <td className="num">{formatMoney(p.price, p.currency)}</td>
                      <td>
                        <StatusBadge status={p.status} />
                      </td>
                      <td className="hide-sm muted small">{formatDate(p.updated_at)}</td>
                      <td className="num actions">
                        <button className="btn btn-sm" onClick={() => setEditing({ mode: "edit", product: p })}>
                          Edit
                        </button>
                        <button className="btn btn-sm btn-danger-outline" onClick={() => archive(p)}>
                          Archive
                        </button>
                      </td>
                    </tr>
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

function ProductForm({
  product,
  onCancel,
  onSaved,
}: {
  product: Product | null;
  onCancel: () => void;
  onSaved: (p: Product, created: boolean) => void;
}) {
  const [sku, setSku] = useState(product?.sku ?? "");
  const [name, setName] = useState(product?.name ?? "");
  const [description, setDescription] = useState(product?.description ?? "");
  const [currency, setCurrency] = useState(product?.currency ?? "INR");
  const [price, setPrice] = useState(product ? toMajorString(product.price, product.currency) : "");
  const [status, setStatus] = useState<"ACTIVE" | "INACTIVE">(product?.status === "INACTIVE" ? "INACTIVE" : "ACTIVE");
  const [error, setError] = useState<unknown>();
  const [saving, setSaving] = useState(false);

  const onSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setSaving(true);
    setError(undefined);
    const cur = currency.trim().toUpperCase();
    const minor = toMinorUnits(price, cur || "INR");
    try {
      if (!product) {
        const input: ProductInput = { sku, name, description, price: Number.isNaN(minor) ? 0 : minor, currency: cur, status };
        onSaved(await productsApi.create(input), true);
      } else {
        // PATCH only what changed.
        const patch: ProductPatch = {};
        if (name !== product.name) patch.name = name;
        if (description !== product.description) patch.description = description;
        if (cur !== product.currency) patch.currency = cur;
        if (minor !== product.price) patch.price = Number.isNaN(minor) ? 0 : minor;
        if (status !== product.status) patch.status = status;
        if (Object.keys(patch).length === 0) return onSaved(product, false);
        onSaved(await productsApi.update(product.id, patch), false);
      }
    } catch (err) {
      setError(err);
    } finally {
      setSaving(false);
    }
  };

  const fields = error instanceof ApiError ? error.fields : {};
  return (
    <form className="card form-card" onSubmit={onSubmit} noValidate>
      <h2>{product ? `Edit “${product.name}”` : "New product"}</h2>
      {error !== undefined && (!Object.keys(fields).length || fields.body) && <ErrorAlert error={error} />}
      <div className="form-grid">
        <Field label="SKU" error={fields.sku} hint={product ? "SKUs cannot be changed" : "e.g. MUG-RED-01"}>
          <input value={sku} onChange={(e) => setSku(e.target.value)} disabled={!!product} required />
        </Field>
        <Field label="Name" error={fields.name}>
          <input value={name} onChange={(e) => setName(e.target.value)} required />
        </Field>
        {/* The backend's message talks about minor units; this form uses normal units. */}
        <Field
          label="Price"
          error={fields.price && "Enter a price greater than 0, e.g. 1299.00"}
          hint="In the currency's normal units, e.g. 1299.00"
        >
          <input inputMode="decimal" value={price} onChange={(e) => setPrice(e.target.value)} required />
        </Field>
        <Field label="Currency" error={fields.currency} hint="3-letter code">
          <input value={currency} maxLength={3} onChange={(e) => setCurrency(e.target.value.toUpperCase())} required />
        </Field>
        <Field label="Status" error={fields.status}>
          <select value={status} onChange={(e) => setStatus(e.target.value as "ACTIVE" | "INACTIVE")}>
            <option value="ACTIVE">Active (for sale)</option>
            <option value="INACTIVE">Inactive (hidden)</option>
          </select>
        </Field>
        <Field label="Description" error={fields.description}>
          <textarea rows={3} value={description} onChange={(e) => setDescription(e.target.value)} />
        </Field>
      </div>
      <div className="form-actions">
        <button type="button" className="btn btn-ghost" onClick={onCancel}>
          Cancel
        </button>
        <button className="btn btn-primary" disabled={saving}>
          {saving ? "Saving…" : product ? "Save changes" : "Create product"}
        </button>
      </div>
      {!product && <p className="muted small">New products start with 0 in stock - add stock under Inventory.</p>}
    </form>
  );
}
