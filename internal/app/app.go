// Package app builds the complete application: it creates every module's
// repository, service and handler and connects them (dependency injection).
//
// Both cmd/api (production) and the API tests call NewHandler, so the tests
// exercise exactly the same wiring that runs in production.
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

// NewHandler wires all modules together and returns the HTTP handler.
func NewHandler(cfg config.Config, pool *pgxpool.Pool, logger *slog.Logger, opts Options) (http.Handler, error) {
	if opts.BcryptCost == 0 {
		opts.BcryptCost = bcrypt.DefaultCost
	}

	// Repositories (database access)
	userRepo := users.NewRepository(pool)
	productRepo := products.NewRepository(pool)
	inventoryRepo := inventory.NewRepository(pool)
	orderRepo := orders.NewRepository(pool)

	// Services (business logic)
	tokens := auth.NewTokenManager(cfg.JWTSecret, cfg.JWTTTL)
	authService, err := auth.NewService(userRepo, tokens, auth.NewPasswordHasher(opts.BcryptCost))
	if err != nil {
		return nil, err
	}
	productService := products.NewService(pool, productRepo, inventoryRepo)
	inventoryService := inventory.NewService(pool, inventoryRepo)
	orderService := orders.NewService(pool, orderRepo, productRepo)

	// Handlers (HTTP)
	return server.NewRouter(server.Deps{
		Logger: logger,
		Health: health.NewHandler(map[string]health.CheckFunc{
			"postgres": pool.Ping,
		}),
		Auth:        auth.NewHandler(authService),
		Users:       users.NewHandler(userRepo),
		Products:    products.NewHandler(productService),
		Inventory:   inventory.NewHandler(inventoryService),
		Orders:      orders.NewHandler(orderService),
		RequireAuth: auth.RequireAuth(tokens),
	}), nil
}
