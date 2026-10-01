import { Link } from "react-router";
import { ordersApi, productsApi, systemApi } from "../../api/services";
import { AdminNav } from "../../components/Layout";
import { EmptyState, ErrorAlert, PageHeader, Spinner, StatusBadge } from "../../components/ui";
import { useAsync } from "../../hooks/useAsync";
import type { OrderStatus } from "../../types/api";
import { ORDER_STATUS_LABEL, formatDate, formatMoney } from "../../utils/format";

const TRACKED: OrderStatus[] = ["RESERVED", "PAYMENT_PENDING", "CONFIRMED", "CANCELLED", "EXPIRED"];

/**
 * Everything here comes from existing endpoints. Counts use the list
 * endpoints' `total` with limit=1, so each number costs one tiny request.
 */
async function loadDashboard(signal: AbortSignal) {
  const count = (status: OrderStatus) => ordersApi.list({ status, limit: 1 }, signal).then((p) => p.total);
  const [ready, activeProducts, inactiveProducts, recent, ...statusCounts] = await Promise.all([
    systemApi.ready(),
    productsApi.list({ status: "ACTIVE", limit: 1 }, signal).then((p) => p.total),
    productsApi.list({ status: "INACTIVE", limit: 1 }, signal).then((p) => p.total),
    ordersApi.list({ limit: 6 }, signal),
    ...TRACKED.map(count),
  ]);
  return {
    ready,
    activeProducts,
    inactiveProducts,
    recent,
    byStatus: Object.fromEntries(TRACKED.map((s, i) => [s, statusCounts[i]])) as Record<OrderStatus, number>,
  };
}

export function AdminDashboardPage() {
  const { data, error, loading, reload } = useAsync(loadDashboard, []);

  return (
    <>
      <PageHeader
        title="Admin dashboard"
        actions={
          <button className="btn btn-sm" onClick={reload} disabled={loading}>
            Refresh
          </button>
        }
      />
      <AdminNav />

      {error !== undefined ? (
        <ErrorAlert error={error} onRetry={reload} />
      ) : !data ? (
        <Spinner label="Loading dashboard…" />
      ) : (
        <>
          <div className="stats">
            <Stat label="Total orders" value={data.recent.total} to="/orders" />
            <Stat label="Awaiting payment" value={data.byStatus.RESERVED} to="/orders?status=RESERVED" />
            <Stat label="Payment pending" value={data.byStatus.PAYMENT_PENDING} to="/orders?status=PAYMENT_PENDING" tone="warning" />
            <Stat label="Confirmed" value={data.byStatus.CONFIRMED} to="/orders?status=CONFIRMED" tone="success" />
            <Stat label="Cancelled" value={data.byStatus.CANCELLED} to="/orders?status=CANCELLED" />
            <Stat label="Expired" value={data.byStatus.EXPIRED} to="/orders?status=EXPIRED" />
            <Stat label="Active products" value={data.activeProducts} to="/admin/products" />
            <Stat label="Inactive products" value={data.inactiveProducts} to="/admin/products?status=INACTIVE" />
          </div>

          <div className="cart-layout">
            <div className="card">
              <div className="row-between">
                <h2>Recent orders</h2>
                <Link to="/orders">View all →</Link>
              </div>
              {data.recent.items.length === 0 ? (
                <EmptyState title="No orders yet" />
              ) : (
                <table className="table">
                  <tbody>
                    {data.recent.items.map((o) => (
                      <tr key={o.id}>
                        <td>
                          <Link to={`/orders/${o.id}`}>#{o.id}</Link>
                          <div className="muted small">{formatDate(o.created_at)}</div>
                        </td>
                        <td>
                          <StatusBadge status={o.status} label={ORDER_STATUS_LABEL[o.status]} />
                        </td>
                        <td className="num">{formatMoney(o.total_amount, o.currency)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </div>

            <aside className="card summary">
              <h2>System status</h2>
              <p>
                <StatusBadge
                  status={data.ready.status === "ready" ? "ACTIVE" : data.ready.status === "degraded" ? "PENDING" : "FAILED"}
                  label={data.ready.status.replace("_", " ")}
                />
              </p>
              <dl className="kv">
                {Object.entries(data.ready.checks).map(([name, state]) => (
                  <div key={name} className="kv-row">
                    <dt>{name}</dt>
                    <dd>{state}</dd>
                  </div>
                ))}
              </dl>
              <p className="muted small">From the backend's /ready endpoint.</p>
            </aside>
          </div>
        </>
      )}
    </>
  );
}

function Stat({ label, value, to, tone }: { label: string; value: number; to: string; tone?: "success" | "warning" }) {
  return (
    <Link to={to} className={`card stat ${tone ? `stat-${tone}` : ""}`}>
      <span className="stat-value">{value}</span>
      <span className="stat-label">{label}</span>
    </Link>
  );
}
