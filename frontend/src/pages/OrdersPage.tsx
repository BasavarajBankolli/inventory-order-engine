import { Link, useNavigate, useSearchParams } from "react-router";
import { ordersApi } from "../api/services";
import { useAuth } from "../auth/AuthContext";
import { EmptyState, ErrorAlert, PageHeader, Pagination, Spinner, StatusBadge } from "../components/ui";
import { useAsync } from "../hooks/useAsync";
import type { OrderStatus } from "../types/api";
import { ORDER_STATUS_LABEL, formatDate, formatMoney } from "../utils/format";

const PAGE_SIZE = 15;
const STATUSES = Object.keys(ORDER_STATUS_LABEL) as OrderStatus[];

export function OrdersPage() {
  const { isAdmin } = useAuth();
  const navigate = useNavigate();
  const [params, setParams] = useSearchParams();
  const status = (params.get("status") as OrderStatus) || undefined;
  const offset = Number(params.get("offset")) || 0;

  const { data, error, loading, reload } = useAsync(
    (signal) => ordersApi.list({ status, limit: PAGE_SIZE, offset }, signal),
    [status, offset],
  );

  const setFilter = (next: { status?: string; offset?: number }) => {
    const p = new URLSearchParams();
    const s = next.status ?? status;
    if (s) p.set("status", s);
    if (next.offset) p.set("offset", String(next.offset));
    setParams(p, { replace: true });
  };

  return (
    <>
      <PageHeader
        title={isAdmin ? "All orders" : "My orders"}
        subtitle={isAdmin ? "Every customer's orders (admin view)." : "Track your orders and complete pending payments."}
      />

      <div className="toolbar">
        <select
          value={status ?? ""}
          onChange={(e) => setFilter({ status: e.target.value, offset: 0 })}
          aria-label="Filter by status"
        >
          <option value="">All statuses</option>
          {STATUSES.map((s) => (
            <option key={s} value={s}>
              {ORDER_STATUS_LABEL[s]}
            </option>
          ))}
        </select>
      </div>

      {error !== undefined ? (
        <ErrorAlert error={error} onRetry={reload} />
      ) : loading && !data ? (
        <Spinner label="Loading orders…" />
      ) : data && data.items.length === 0 ? (
        <EmptyState
          title={status ? "No orders with this status" : "No orders yet"}
          text={status ? undefined : "When you place an order, it will show up here."}
          action={!status && <Link to="/products" className="btn btn-primary">Start shopping</Link>}
        />
      ) : (
        data && (
          <>
            <div className={`card table-card ${loading ? "is-loading" : ""}`}>
              <table className="table table-clickable">
                <thead>
                  <tr>
                    <th>Order</th>
                    {isAdmin && <th>Customer</th>}
                    <th>Placed</th>
                    <th>Status</th>
                    <th className="num">Total</th>
                  </tr>
                </thead>
                <tbody>
                  {data.items.map((o) => (
                    <tr key={o.id} onClick={() => navigate(`/orders/${o.id}`)}>
                      <td>
                        <Link to={`/orders/${o.id}`} onClick={(e) => e.stopPropagation()}>
                          #{o.id}
                        </Link>
                      </td>
                      {isAdmin && <td className="muted">User #{o.user_id}</td>}
                      <td>{formatDate(o.created_at)}</td>
                      <td>
                        <StatusBadge status={o.status} label={ORDER_STATUS_LABEL[o.status]} />
                      </td>
                      <td className="num">{formatMoney(o.total_amount, o.currency)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <Pagination total={data.total} limit={data.limit} offset={data.offset} onChange={(o) => setFilter({ offset: o })} />
          </>
        )
      )}
    </>
  );
}
