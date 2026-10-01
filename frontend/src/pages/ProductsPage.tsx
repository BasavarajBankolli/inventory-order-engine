import { useEffect, useState } from "react";
import { useSearchParams } from "react-router";
import { productsApi } from "../api/services";
import { ProductCard } from "../components/ProductCard";
import { EmptyState, ErrorAlert, PageHeader, Pagination, Spinner } from "../components/ui";
import { useAsync, useDebounced } from "../hooks/useAsync";
import type { ProductSort } from "../types/api";

const PAGE_SIZE = 12;

const SORTS: { value: ProductSort; label: string }[] = [
  { value: "newest", label: "Newest" },
  { value: "price_asc", label: "Price: low to high" },
  { value: "price_desc", label: "Price: high to low" },
  { value: "name", label: "Name A-Z" },
];

export function ProductsPage() {
  // Search, sort and page live in the URL, so they survive reloads and can
  // be shared as links.
  const [params, setParams] = useSearchParams();
  const sort = (params.get("sort") as ProductSort) || "newest";
  const offset = Number(params.get("offset")) || 0;
  const q = params.get("q") ?? "";
  const [search, setSearch] = useState(q);
  const debouncedSearch = useDebounced(search.trim());

  const update = (changes: Record<string, string | number | null>) => {
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        for (const [k, v] of Object.entries(changes)) {
          if (v === null || v === "" || v === 0) next.delete(k);
          else next.set(k, String(v));
        }
        return next;
      },
      { replace: true },
    );
  };

  // When the (debounced) search text changes, put it in the URL and go back
  // to page 1. Done in an effect: changing the URL during render is not allowed.
  useEffect(() => {
    if (debouncedSearch !== q) update({ q: debouncedSearch || null, offset: null });
  }, [debouncedSearch]);

  const { data, error, loading, reload } = useAsync(
    (signal) => productsApi.list({ q, sort, limit: PAGE_SIZE, offset }, signal),
    [q, sort, offset],
  );

  return (
    <>
      <PageHeader title="Shop" subtitle="Browse our catalogue and order in a few clicks." />

      <div className="toolbar">
        <input
          type="search"
          className="search"
          placeholder="Search by name or SKU…"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          aria-label="Search products"
        />
        <select value={sort} onChange={(e) => update({ sort: e.target.value, offset: null })} aria-label="Sort">
          {SORTS.map((s) => (
            <option key={s.value} value={s.value}>
              {s.label}
            </option>
          ))}
        </select>
      </div>

      {error !== undefined ? (
        <ErrorAlert error={error} onRetry={reload} />
      ) : loading && !data ? (
        <Spinner label="Loading products…" />
      ) : data && data.items.length === 0 ? (
        <EmptyState
          title={q ? `No products match “${q}”` : "No products yet"}
          text={q ? "Try a different search." : "Check back soon."}
        />
      ) : (
        data && (
          <>
            <div className={`grid products-grid ${loading ? "is-loading" : ""}`}>
              {data.items.map((p) => (
                <ProductCard key={p.id} product={p} />
              ))}
            </div>
            <Pagination total={data.total} limit={data.limit} offset={data.offset} onChange={(o) => update({ offset: o })} />
          </>
        )
      )}
    </>
  );
}
