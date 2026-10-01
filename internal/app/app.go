// Package app builds the complete application: it creates every module's
// repository, service and handler and connects them (dependency injection).
//
// cmd/api calls NewHandler, cmd/worker calls NewWorkerJobs, and the API tests
// call NewHandler too - so tests exercise exactly the wiring that runs in
// production, and the API and the worker share one definition of it.
package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"

	"inventory-order-engine/internal/auth"
	"inventory-order-engine/internal/cache"
	"inventory-order-engine/internal/config"
	"inventory-order-engine/internal/events"
	"inventory-order-engine/internal/health"
	"inventory-order-engine/internal/inventory"
	"inventory-order-engine/internal/metrics"
	"inventory-order-engine/internal/orders"
	"inventory-order-engine/internal/payments"
	"inventory-order-engine/internal/products"
	"inventory-order-engine/internal/ratelimit"
	"inventory-order-engine/internal/server"
	"inventory-order-engine/internal/users"
)

// Options lets tests tweak a few things that production never changes.
type Options struct {
	// BcryptCost defaults to bcrypt.DefaultCost. Tests use bcrypt.MinCost
	// so that hashing thousands of passwords stays fast.
	BcryptCost int

	// Redis is optional. nil = no product cache and no rate limiting.
	Redis *redis.Client

	// RedisKeyPrefix keeps keys of parallel tests apart. Production: "".
	RedisKeyPrefix string

	// Metrics is optional. nil = no metrics recorded and no /metrics.
	Metrics *metrics.Metrics
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

	// The product cache is only used when Redis is configured. A nil
	// products.Cache means "always read PostgreSQL".
	var productCache products.Cache
	if opts.Redis != nil {
		productCache = cache.NewProductCache(opts.Redis, cfg.ProductCacheTTL, opts.RedisKeyPrefix)
	}

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
		Outbox:         events.NewOutbox(pool),
		ReservationTTL: cfg.ReservationTTL,
		PaymentTimeout: cfg.PaymentTimeout,
		ReconcileAfter: cfg.PaymentReconcileAfter,
		Metrics:        opts.Metrics,
	})

	return &services{
		userRepo:  userRepo,
		tokens:    tokens,
		auth:      authService,
		products:  products.NewService(pool, productRepo, inventoryRepo, productCache, opts.Metrics),
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

	checks := []health.Check{{Name: "postgres", Fn: pool.Ping, Required: true}}
	deps := server.Deps{
		Logger:  logger,
		Metrics: opts.Metrics,

		CORSAllowedOrigins: cfg.CORSAllowedOrigins,
		MetricsToken:       cfg.MetricsToken,
		Auth:               auth.NewHandler(s.auth),
		Users:              users.NewHandler(s.userRepo),
		Products:           products.NewHandler(s.products),
		Inventory:          inventory.NewHandler(s.inventory),
		Orders:             orders.NewHandler(s.orders),
		RequireAuth:        auth.RequireAuth(s.tokens),
	}

	if opts.Redis != nil {
		// Optional dependency: Redis down => /ready says "degraded", not 503.
		checks = append(checks, health.Check{Name: "redis", Required: false, Fn: func(ctx context.Context) error {
			return opts.Redis.Ping(ctx).Err()
		}})

		if cfg.RateLimitPerMinute > 0 {
			limiter := ratelimit.NewLimiter(opts.Redis, cfg.RateLimitPerMinute, time.Minute, opts.RedisKeyPrefix)
			deps.RateLimitByIP = ratelimit.Middleware(limiter, ratelimit.ByIP(cfg.TrustedProxyHops), opts.Metrics)
			deps.RateLimitByUser = ratelimit.Middleware(limiter, ratelimit.ByUser(cfg.TrustedProxyHops), opts.Metrics)
		}
	}
	deps.Health = health.NewHandler(checks...)

	return server.NewRouter(deps), nil
}
