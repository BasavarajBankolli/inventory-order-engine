import { Link, useNavigate } from "react-router";
import { authApi } from "../api/services";
import { useAuth } from "../auth/AuthContext";
import { ErrorAlert, PageHeader, Spinner, StatusBadge } from "../components/ui";
import { useAsync } from "../hooks/useAsync";
import { formatDate } from "../utils/format";

export function ProfilePage() {
  const { logout: endSession } = useAuth();
  const navigate = useNavigate();
  const logout = () => {
    endSession();
    navigate("/products");
  };
  // Always load fresh data from the backend (GET /users/me).
  const { data: user, error, loading, reload } = useAsync(() => authApi.me(), []);

  if (loading) return <Spinner />;
  if (error !== undefined || !user) return <ErrorAlert error={error} onRetry={reload} />;

  return (
    <>
      <PageHeader title="Your profile" />
      <div className="card profile">
        <div className="avatar" aria-hidden="true">
          {user.name.slice(0, 1).toUpperCase()}
        </div>
        <dl className="kv">
          <dt>Name</dt>
          <dd>{user.name}</dd>
          <dt>Email</dt>
          <dd>{user.email}</dd>
          <dt>Account type</dt>
          <dd>
            <StatusBadge status={user.role === "ADMIN" ? "CREATED" : "neutral"} label={user.role === "ADMIN" ? "Administrator" : "Customer"} />
          </dd>
          <dt>Member since</dt>
          <dd>{formatDate(user.created_at)}</dd>
        </dl>
        <div className="profile-actions">
          <Link to="/orders" className="btn">
            View orders
          </Link>
          {user.role === "ADMIN" && (
            <Link to="/admin" className="btn">
              Admin dashboard
            </Link>
          )}
          <button className="btn btn-danger-outline" onClick={logout}>
            Log out
          </button>
        </div>
      </div>
    </>
  );
}
