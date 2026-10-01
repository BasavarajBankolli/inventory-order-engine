import { useState } from "react";
import { Link, NavLink, Outlet, useLocation, useNavigate } from "react-router";
import { useAuth } from "../auth/AuthContext";
import { useCart } from "../cart/CartContext";

export function Layout() {
  const { user, isAdmin, logout } = useAuth();
  const { count } = useCart();
  const navigate = useNavigate();
  const location = useLocation();
  const [menuOpen, setMenuOpen] = useState(false);
  const [lastPath, setLastPath] = useState(location.pathname);

  // Close the mobile menu after navigating.
  if (location.pathname !== lastPath) {
    setLastPath(location.pathname);
    setMenuOpen(false);
  }

  const onLogout = () => {
    logout();
    navigate("/products");
  };

  const navClass = ({ isActive }: { isActive: boolean }) => (isActive ? "nav-link active" : "nav-link");

  return (
    <div className="app">
      <header className="topbar">
        <div className="container topbar-inner">
          <Link to="/products" className="brand">
            <span className="brand-mark" aria-hidden="true" />
            StockFlow
          </Link>

          <button
            className="menu-toggle"
            aria-label="Toggle navigation"
            aria-expanded={menuOpen}
            onClick={() => setMenuOpen((o) => !o)}
          >
            ☰
          </button>

          <nav className={`nav ${menuOpen ? "open" : ""}`}>
            <NavLink to="/products" className={navClass}>
              Shop
            </NavLink>
            {user && (
              <NavLink to="/orders" className={navClass}>
                {isAdmin ? "All orders" : "My orders"}
              </NavLink>
            )}
            {isAdmin && (
              <NavLink to="/admin" className={navClass}>
                Admin
              </NavLink>
            )}
            <NavLink to="/cart" className={navClass}>
              Cart{count > 0 && <span className="cart-count">{count}</span>}
            </NavLink>
            <span className="nav-sep" />
            {user ? (
              <>
                <NavLink to="/profile" className={navClass}>
                  {user.name}
                </NavLink>
                <button className="btn btn-sm btn-ghost" onClick={onLogout}>
                  Log out
                </button>
              </>
            ) : (
              <>
                <NavLink to="/login" className={navClass}>
                  Log in
                </NavLink>
                <Link to="/register" className="btn btn-sm btn-primary">
                  Sign up
                </Link>
              </>
            )}
          </nav>
        </div>
      </header>

      <main className="container main">
        <Outlet />
      </main>

      <footer className="footer">
        <div className="container muted small">StockFlow · Inventory &amp; Order Management demo</div>
      </footer>
    </div>
  );
}

export function AdminNav() {
  return (
    <div className="tabs">
      <NavLink end to="/admin" className={({ isActive }) => (isActive ? "tab active" : "tab")}>
        Dashboard
      </NavLink>
      <NavLink to="/admin/products" className={({ isActive }) => (isActive ? "tab active" : "tab")}>
        Products
      </NavLink>
      <NavLink to="/admin/inventory" className={({ isActive }) => (isActive ? "tab active" : "tab")}>
        Inventory
      </NavLink>
      <NavLink to="/orders" className={({ isActive }) => (isActive ? "tab active" : "tab")}>
        Orders
      </NavLink>
    </div>
  );
}
