import { Link, Navigate, Route, Routes } from "react-router";
import { RequireAuth } from "./auth/RequireAuth";
import { Layout } from "./components/Layout";
import { EmptyState } from "./components/ui";
import { AdminDashboardPage } from "./pages/admin/AdminDashboardPage";
import { AdminInventoryPage } from "./pages/admin/AdminInventoryPage";
import { AdminProductsPage } from "./pages/admin/AdminProductsPage";
import { LoginPage, RegisterPage } from "./pages/AuthPages";
import { CartPage } from "./pages/CartPage";
import { CheckoutPage } from "./pages/CheckoutPage";
import { OrderDetailsPage } from "./pages/OrderDetailsPage";
import { OrdersPage } from "./pages/OrdersPage";
import { ProductDetailsPage } from "./pages/ProductDetailsPage";
import { ProductsPage } from "./pages/ProductsPage";
import { ProfilePage } from "./pages/ProfilePage";

export function App() {
  return (
    <Routes>
      <Route element={<Layout />}>
        <Route index element={<Navigate to="/products" replace />} />

        {/* Public */}
        <Route path="login" element={<LoginPage />} />
        <Route path="register" element={<RegisterPage />} />
        <Route path="products" element={<ProductsPage />} />
        <Route path="products/:id" element={<ProductDetailsPage />} />
        <Route path="cart" element={<CartPage />} />

        {/* Logged in */}
        <Route path="checkout" element={<RequireAuth><CheckoutPage /></RequireAuth>} />
        <Route path="orders" element={<RequireAuth><OrdersPage /></RequireAuth>} />
        <Route path="orders/:id" element={<RequireAuth><OrderDetailsPage /></RequireAuth>} />
        <Route path="profile" element={<RequireAuth><ProfilePage /></RequireAuth>} />

        {/* Admins */}
        <Route path="admin" element={<RequireAuth admin><AdminDashboardPage /></RequireAuth>} />
        <Route path="admin/products" element={<RequireAuth admin><AdminProductsPage /></RequireAuth>} />
        <Route path="admin/inventory" element={<RequireAuth admin><AdminInventoryPage /></RequireAuth>} />

        <Route
          path="*"
          element={<EmptyState title="Page not found" action={<Link to="/products" className="btn">Go to the shop</Link>} />}
        />
      </Route>
    </Routes>
  );
}
