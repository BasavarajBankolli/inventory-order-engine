// Package app builds the complete application: it creates every module's
// repository, service and handler and connects them (dependency injection).
//
// cmd/api calls NewHandler, cmd/worker calls NewWorkerJobs, and the API tests
// call NewHandler too - so tests exercise exactly the wiring that runs in
// production, and the API and the worker share one definition of it.
package app

import (
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"inventory-order-engine/internal/auth"
	"inventory-order-engine/internal/config"
	"inventory-order-engine/internal/health"
	"inventory-order-engine/internal/inventory"
	"inventory-order-engine/internal/orders"
	"inventory-order-engine/internal/payments"
	"inventory-order-engine/internal/products"
	"inventory-order-engine/internal/server"
	"inventory-order-engine/internal/users"
)

// Options lets tests tweak a few things that production never changes.
type Options struct {
	// BcryptCost defaults to bcrypt.DefaultCost. Tests use bcrypt.MinCost
	// so that hashing thousands of passwords stays fast.
	BcryptCost int
}

// services holds every business service, built once.
type services struct {
	userRepo  *users.Repository
	tokens    *auth.TokenManager
	auth      *auth.Service
	products  *products.Service
	inventory *inventory.Service
	orders    *orders.Service
}

func newServices(cfg config.Config, pool *pgxpool.Pool, opts Options) (*services, error) {
	if opts.BcryptCost == 0 {
		opts.BcryptCost = bcrypt.DefaultCost
	}

	// Repositories (database access)
	userRepo := users.NewRepository(pool)
	productRepo := products.NewRepository(pool)
	inventoryRepo := inventory.NewRepository(pool)

	// Services (business logic)
	tokens := auth.NewTokenManager(cfg.JWTSecret, cfg.JWTTTL)
	authService, err := auth.NewService(userRepo, tokens, auth.NewPasswordHasher(opts.BcryptCost))
	if err != nil {
		return nil, err
	}
	inventoryService := inventory.NewService(pool, inventoryRepo)

	// The only provider implementation is the mock. A real one (Stripe,
	// Razorpay, ...) would be chosen here from configuration.
	outcome, _ := payments.ParseOutcome(cfg.MockPaymentOutcome) // validated by config
	orderService := orders.NewService(orders.Deps{
		Pool:           pool,
		Orders:         orders.NewRepository(pool),
		Products:       productRepo,
		Inventory:      inventoryService,
		Payments:       payments.NewRepository(pool),
		Provider:       payments.NewMockProvider(pool, outcome),
		ReservationTTL: cfg.ReservationTTL,
		PaymentTimeout: cfg.PaymentTimeout,
		ReconcileAfter: cfg.PaymentReconcileAfter,
	})

	return &services{
		userRepo:  userRepo,
		tokens:    tokens,
		auth:      authService,
		products:  products.NewService(pool, productRepo, inventoryRepo),
		inventory: inventoryService,
		orders:    orderService,
	}, nil
}

// NewHandler wires all modules together and returns the HTTP handler.
func NewHandler(cfg config.Config, pool *pgxpool.Pool, logger *slog.Logger, opts Options) (http.Handler, error) {
	s, err := newServices(cfg, pool, opts)
	if err != nil {
		return nil, err
	}

	return server.NewRouter(server.Deps{
		Logger: logger,
		Health: health.NewHandler(map[string]health.CheckFunc{
			"postgres": pool.Ping,
		}),
		Auth:        auth.NewHandler(s.auth),
		Users:       users.NewHandler(s.userRepo),
		Products:    products.NewHandler(s.products),
		Inventory:   inventory.NewHandler(s.inventory),
		Orders:      orders.NewHandler(s.orders),
		RequireAuth: auth.RequireAuth(s.tokens),
	}), nil
}
