# StockFlow — Frontend

A React + TypeScript web app for the **Inventory & Order Management** backend in the parent folder.
Customers browse, order and pay; admins manage products, stock and orders. Every screen uses the
real backend API. There is no mock data.

| | |
|---|---|
| Stack | React 19, TypeScript 7, Vite 8, React Router 8, plain CSS, `fetch` |
| Pages | Login, Register, Products, Product details, Cart, Checkout, My orders, Order details, Profile, Admin dashboard, Admin products, Admin inventory |
| Size | ~95 KB gzipped JavaScript, ~3 KB gzipped CSS |

## 1. Run it locally

You need the backend running (from the repository root) and Node.js 20+.

```powershell
# terminal 1: backend (API on http://localhost:8080)
docker compose up --build -d

# terminal 2: frontend (http://localhost:5173)
cd frontend
npm install
Copy-Item .env.example .env        # optional: the default is already http://localhost:8080
npm run dev
```

Open <http://localhost:5173>.

Test accounts: register a new one in the app. To get an **admin**, register and then promote the
account in the database (the API deliberately can't do this), and log in again:

```powershell
docker compose exec postgres psql -U app -d inventory -c "UPDATE users SET role = 'ADMIN' WHERE email = 'you@example.com';"
```

Other scripts:

| Command | What it does |
|---|---|
| `npm run build` | Type-check (`tsc`) and build the production bundle into `dist/` |
| `npm run preview` | Serve `dist/` on <http://localhost:4173> to try the production build |
| `npm run typecheck` | Type-check only |

## 2. How it connects to the backend

```text
Browser (React app) ──fetch──► VITE_API_BASE_URL + /api/v1/...   (Go API)
                      Authorization: Bearer <JWT>
```

- **One HTTP client**: `src/api/client.ts` adds the base URL and the `Authorization` header, parses the backend's error format `{"error":{code,message,fields,request_id}}` into an `ApiError`, turns network failures into a clear message, and handles `401` (logs you out) and `429` (tells you how long to wait).
- **One function per endpoint**: `src/api/services.ts` (auth, products, inventory, orders, payments, `/ready`). Pages never call `fetch` directly.
- **Authentication**: `POST /api/v1/auth/login` returns a JWT. The app keeps it in `localStorage`, sends it on every request, re-validates it with `GET /users/me` on page load, and logs out automatically when it expires. Route guards (`RequireAuth`) only improve the UX; the backend enforces every rule itself (401/403).
- **Cart**: the backend has no cart API, so the cart lives in `localStorage`. The cart page re-checks every product against the API (current price, still for sale).
- **Checkout**: `POST /api/v1/orders` with an **`Idempotency-Key`** (one per cart version). A double-click or network retry returns the same order instead of creating two. The backend reserves the stock in the same step, and `409 OUT_OF_STOCK` is shown with the exact available quantity.
- **Payment**: on the order page, `POST /api/v1/orders/{id}/pay`. The backend's payment provider is a mock, so the page lets you choose its behaviour (approved / declined / timeout):
  - **declined** (402) → the order is cancelled and the stock released; you can put the items back in your cart;
  - **timeout** (504) → the order stays pending and "Retry payment" is safe (you're never charged twice).
- **Admin**:
  - dashboard: counts from the list endpoints' `total`, recent orders, and live `/ready` status;
  - products: create, edit (only changed fields are sent), archive;
  - inventory: relative adjustments, and exact counts using the backend's `version` for optimistic locking. A conflicting change is detected, and the row reloads.

### CORS (the one backend change)

A browser only lets the page read API responses if the API allows the page's **origin**. The
backend reads the allowed origins from `CORS_ALLOWED_ORIGINS` (comma-separated). Docker Compose
allows `http://localhost:5173` and `http://localhost:4173` by default. **In production, add your
Vercel URL there**, otherwise every request fails with a CORS error in the browser console.

## 3. Environment variables

| Variable | Where | Example | Notes |
|---|---|---|---|
| `VITE_API_BASE_URL` | frontend (`.env`, Vercel project settings) | `http://localhost:8080` / `https://your-api.onrender.com` | No trailing slash. Baked into the JavaScript **at build time**, so changing it requires a rebuild or redeploy. Defaults to `http://localhost:8080` |
| `CORS_ALLOWED_ORIGINS` | **backend** (Render) | `https://stockflow.vercel.app` | Must include the frontend's exact origin (scheme + host, no path) |

`VITE_*` variables are public: they end up in the browser bundle. Never put secrets in them.

## 4. Deploy to Vercel

The repository contains both the backend and the frontend, so tell Vercel to build the `frontend`
folder.

1. Push the repository to GitHub.
2. In Vercel: **Add New → Project → import the repository**.
3. Set **Root Directory** to `frontend`. The framework is detected as **Vite**; the build command `npm run build` and output `dist` come from `vercel.json`.
4. Under **Environment Variables** add `VITE_API_BASE_URL` = your Render backend URL, e.g. `https://inventory-api.onrender.com`.
5. Click **Deploy**. You get a URL like `https://stockflow-yourname.vercel.app`.
6. On **Render** (backend service → Environment), set `CORS_ALLOWED_ORIGINS=https://stockflow-yourname.vercel.app` and redeploy the backend.

`vercel.json` rewrites every path to `index.html`, so deep links like `/orders/42` work after a
page reload. That's needed for any single-page app using client-side routing.

Checklist if something doesn't work after deploying:

| Symptom | Fix |
|---|---|
| "Cannot reach the server at http://localhost:8080" in production | `VITE_API_BASE_URL` wasn't set when Vercel built. Set it and **redeploy** |
| Browser console: *blocked by CORS policy* | Add the exact Vercel origin to `CORS_ALLOWED_ORIGINS` on Render |
| 404 when reloading `/orders/5` | `vercel.json` is missing, or the Root Directory isn't `frontend` |
| Mixed-content error | The API URL must be `https://` when the site is served over https |
| First request after a while is very slow | Render's free tier sleeps idle services. The first request wakes it up |

## Project structure

```text
src/
  api/client.ts         fetch wrapper: base URL, token, ApiError, 401/429 handling
  api/services.ts       one function per backend endpoint
  types/api.ts          TypeScript types matching the backend JSON
  auth/                 AuthContext (JWT session) and RequireAuth (route guard)
  cart/CartContext.tsx  cart in localStorage, with the backend's limits
  components/           Layout/nav, ProductCard, shared UI (errors, empty states, badges, pagination, fields)
  hooks/useAsync.ts     loading/error/data for API calls (+ cancellation), debounce
  pages/                one file per page (admin pages in pages/admin/)
  utils/format.ts       money (minor units ↔ display), dates, order status texts
  styles.css            the whole design: tokens, components, responsive rules
```
