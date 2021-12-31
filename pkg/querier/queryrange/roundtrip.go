// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/prometheus/prometheus/web/api/v1/api.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Prometheus Authors.

package queryrange

import (
	"context"
	"flag"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/prometheus/promql"
	"github.com/weaveworks/common/middleware"

	"github.com/grafana/mimir/pkg/chunk/cache"
	"github.com/grafana/mimir/pkg/chunk/storage"
	"github.com/grafana/mimir/pkg/tenant"
	"github.com/grafana/mimir/pkg/util"
)

const (
	day                    = 24 * time.Hour
	queryRangePathSuffix   = "/query_range"
	instantQueryPathSuffix = "/query"
)

var (
	errInvalidShardingStorage = errors.New("query sharding support is only available for blocks storage")
)

// Config for query_range middleware chain.
type Config struct {
	SplitQueriesByInterval time.Duration `yaml:"split_queries_by_interval"`
	AlignQueriesWithStep   bool          `yaml:"align_queries_with_step"`
	ResultsCacheConfig     `yaml:"results_cache"`
	CacheResults           bool `yaml:"cache_results"`
	MaxRetries             int  `yaml:"max_retries"`
	ShardedQueries         bool `yaml:"parallelize_shardable_queries"`
	CacheUnalignedRequests bool `yaml:"cache_unaligned_requests"`
}

// RegisterFlags adds the flags required to config this to the given FlagSet.
func (cfg *Config) RegisterFlags(f *flag.FlagSet) {
	f.IntVar(&cfg.MaxRetries, "querier.max-retries-per-request", 5, "Maximum number of retries for a single request; beyond this, the downstream error is returned.")
	f.DurationVar(&cfg.SplitQueriesByInterval, "querier.split-queries-by-interval", 0, "Split queries by an interval and execute in parallel, 0 disables it. You should use an a multiple of 24 hours (same as the storage bucketing scheme), to avoid queriers downloading and processing the same chunks. This also determines how cache keys are chosen when result caching is enabled")
	f.BoolVar(&cfg.AlignQueriesWithStep, "querier.align-querier-with-step", false, "Mutate incoming queries to align their start and end with their step.")
	f.BoolVar(&cfg.CacheResults, "querier.cache-results", false, "Cache query results.")
	f.BoolVar(&cfg.ShardedQueries, "query-frontend.parallelize-shardable-queries", false, "Perform query parallelizations based on storage sharding configuration and query ASTs. This feature is supported only by the blocks storage engine.")
	f.BoolVar(&cfg.CacheUnalignedRequests, "query-frontend.cache-unaligned-requests", false, "Cache requests that are not step-aligned.")
	cfg.ResultsCacheConfig.RegisterFlags(f)
}

// Validate validates the config.
func (cfg *Config) Validate() error {
	if cfg.CacheResults {
		if cfg.SplitQueriesByInterval <= 0 {
			return errors.New("querier.cache-results may only be enabled in conjunction with querier.split-queries-by-interval. Please set the latter")
		}
		if err := cfg.ResultsCacheConfig.Validate(); err != nil {
			return errors.Wrap(err, "invalid ResultsCache config")
		}
	}
	return nil
}

// HandlerFunc is like http.HandlerFunc, but for Handler.
type HandlerFunc func(context.Context, Request) (Response, error)

// Do implements Handler.
func (q HandlerFunc) Do(ctx context.Context, req Request) (Response, error) {
	return q(ctx, req)
}

// Handler is like http.Handle, but specifically for Prometheus query_range calls.
type Handler interface {
	Do(context.Context, Request) (Response, error)
}

// MiddlewareFunc is like http.HandlerFunc, but for Middleware.
type MiddlewareFunc func(Handler) Handler

// Wrap implements Middleware.
func (q MiddlewareFunc) Wrap(h Handler) Handler {
	return q(h)
}

// Middleware is a higher order Handler.
type Middleware interface {
	Wrap(Handler) Handler
}

// MergeMiddlewares produces a middleware that applies multiple middleware in turn;
// ie Merge(f,g,h).Wrap(handler) == f.Wrap(g.Wrap(h.Wrap(handler)))
func MergeMiddlewares(middleware ...Middleware) Middleware {
	return MiddlewareFunc(func(next Handler) Handler {
		for i := len(middleware) - 1; i >= 0; i-- {
			next = middleware[i].Wrap(next)
		}
		return next
	})
}

// Tripperware is a signature for all http client-side middleware.
type Tripperware func(http.RoundTripper) http.RoundTripper

// MergeTripperwares produces a tripperware that applies multiple tripperware in turn;
// ie Merge(f,g,h).Wrap(tripper) == f(g(h(tripper)))
func MergeTripperwares(tripperware ...Tripperware) Tripperware {
	return func(next http.RoundTripper) http.RoundTripper {
		for i := len(tripperware) - 1; i >= 0; i-- {
			next = tripperware[i](next)
		}
		return next
	}
}

// RoundTripFunc is to http.RoundTripper what http.HandlerFunc is to http.Handler.
type RoundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip implements http.RoundTripper.
func (f RoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// NewTripperware returns a Tripperware configured with middlewares to limit, align, split, retry and cache requests.
func NewTripperware(
	cfg Config,
	log log.Logger,
	limits Limits,
	codec Codec,
	cacheExtractor Extractor,
	storageEngine string,
	engineOpts promql.EngineOpts,
	registerer prometheus.Registerer,
	cacheGenNumberLoader CacheGenNumberLoader,
) (Tripperware, cache.Cache, error) {
	queryRangeTripperware, cache, err := newQueryTripperware(cfg, log, limits, codec, cacheExtractor, storageEngine, engineOpts, registerer, cacheGenNumberLoader)
	if err != nil {
		return nil, nil, err
	}
	return MergeTripperwares(
		newActiveUsersTripperware(log, registerer),
		queryRangeTripperware,
	), cache, err
}

func newQueryTripperware(
	cfg Config,
	log log.Logger,
	limits Limits,
	codec Codec,
	cacheExtractor Extractor,
	storageEngine string,
	engineOpts promql.EngineOpts,
	registerer prometheus.Registerer,
	cacheGenNumberLoader CacheGenNumberLoader,
) (Tripperware, cache.Cache, error) {
	// Metric used to keep track of each middleware execution duration.
	metrics := newInstrumentMiddlewareMetrics(registerer)

	queryRangeMiddleware := []Middleware{
		// Track query range statistics. Added first before any subsequent middleware modifies the request.
		newQueryStatsMiddleware(registerer),
		newLimitsMiddleware(limits, log),
	}
	if cfg.AlignQueriesWithStep {
		queryRangeMiddleware = append(queryRangeMiddleware, newInstrumentMiddleware("step_align", metrics, log), newStepAlignMiddleware())
	}

	var c cache.Cache

	// Inject the middleware to split requests by interval + results cache (if at least one of the two is enabled).
	if cfg.SplitQueriesByInterval > 0 || cfg.CacheResults {
		// Init the cache client.
		if cfg.CacheResults {
			var err error

			c, err = cache.New(cfg.ResultsCacheConfig.CacheConfig, registerer, log)
			if err != nil {
				return nil, nil, err
			}
			if cfg.ResultsCacheConfig.Compression == "snappy" {
				c = cache.NewSnappy(c, log)
			}
			if cacheGenNumberLoader != nil {
				c = cache.NewCacheGenNumMiddleware(c)
			}
		}

		shouldCache := func(r Request) bool {
			return !r.GetOptions().CacheDisabled
		}

		queryRangeMiddleware = append(queryRangeMiddleware, newInstrumentMiddleware("split_by_interval_and_results_cache", metrics, log), newSplitAndCacheMiddleware(
			cfg.SplitQueriesByInterval > 0,
			cfg.CacheResults,
			cfg.SplitQueriesByInterval,
			cfg.CacheUnalignedRequests,
			limits,
			codec,
			c,
			constSplitter(cfg.SplitQueriesByInterval),
			cacheExtractor,
			cacheGenNumberLoader,
			shouldCache,
			log,
			registerer,
		))
	}
	queryInstantMiddleware := []Middleware{newLimitsMiddleware(limits, log)}

	if cfg.ShardedQueries {
		if storageEngine != storage.StorageEngineBlocks {
			if c != nil {
				c.Stop()
			}
			return nil, nil, errInvalidShardingStorage
		}

		// Disable concurrency limits for sharded queries.
		engineOpts.ActiveQueryTracker = nil
		queryshardingMiddleware := newQueryShardingMiddleware(
			log,
			promql.NewEngine(engineOpts),
			limits,
			registerer,
		)
		queryRangeMiddleware = append(
			queryRangeMiddleware,
			newInstrumentMiddleware("querysharding", metrics, log),
			queryshardingMiddleware,
		)
		queryInstantMiddleware = append(
			queryInstantMiddleware,
			newInstrumentMiddleware("querysharding", metrics, log),
			queryshardingMiddleware,
		)
	}

	if cfg.MaxRetries > 0 {
		retryMiddlewareMetrics := newRetryMiddlewareMetrics(registerer)
		queryRangeMiddleware = append(queryRangeMiddleware, newInstrumentMiddleware("retry", metrics, log), newRetryMiddleware(log, cfg.MaxRetries, retryMiddlewareMetrics))
		queryInstantMiddleware = append(queryInstantMiddleware, newInstrumentMiddleware("retry", metrics, log), newRetryMiddleware(log, cfg.MaxRetries, retryMiddlewareMetrics))
	}

	return func(next http.RoundTripper) http.RoundTripper {
		queryrange := newLimitedParallelismRoundTripper(next, codec, limits, queryRangeMiddleware...)
		instant := defaultInstantQueryParamsRoundTripper(
			newLimitedParallelismRoundTripper(next, codec, limits, queryInstantMiddleware...),
			time.Now,
		)
		return RoundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch {
			case isRangeQuery(r.URL.Path):
				return queryrange.RoundTrip(r)
			case isInstantQuery(r.URL.Path):
				return instant.RoundTrip(r)
			default:
				return next.RoundTrip(r)
			}
		})
	}, c, nil
}

func newActiveUsersTripperware(logger log.Logger, registerer prometheus.Registerer) Tripperware {
	// Per tenant query metrics.
	queriesPerTenant := promauto.With(registerer).NewCounterVec(prometheus.CounterOpts{
		Name: "cortex_query_frontend_queries_total",
		Help: "Total queries sent per tenant.",
	}, []string{"op", "user"})

	activeUsers := util.NewActiveUsersCleanupWithDefaultValues(func(user string) {
		err := util.DeleteMatchingLabels(queriesPerTenant, map[string]string{"user": user})
		if err != nil {
			level.Warn(logger).Log("msg", "failed to remove cortex_query_frontend_queries_total metric for user", "user", user)
		}
	})

	// Start cleanup. If cleaner stops or fail, we will simply not clean the metrics for inactive users.
	_ = activeUsers.StartAsync(context.Background())
	return func(next http.RoundTripper) http.RoundTripper {
		return RoundTripFunc(func(r *http.Request) (*http.Response, error) {
			op := "query"
			if isRangeQuery(r.URL.Path) {
				op = "query_range"
			}

			tenantIDs, err := tenant.TenantIDs(r.Context())
			// This should never happen anyways because we have auth middleware before this.
			if err != nil {
				return nil, err
			}
			userStr := tenant.JoinTenantIDs(tenantIDs)
			activeUsers.UpdateUserTimestamp(userStr, time.Now())
			queriesPerTenant.WithLabelValues(op, userStr).Inc()

			return next.RoundTrip(r)
		})
	}
}

func isRangeQuery(path string) bool {
	return strings.HasSuffix(path, queryRangePathSuffix)
}

func isInstantQuery(path string) bool {
	return strings.HasSuffix(path, instantQueryPathSuffix)
}

func defaultInstantQueryParamsRoundTripper(next http.RoundTripper, now func() time.Time) http.RoundTripper {
	return RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if isInstantQuery(r.URL.Path) && !r.URL.Query().Has("time") {
			q := r.URL.Query()
			q.Add("time", strconv.FormatInt(time.Now().Unix(), 10))
			r.URL.RawQuery = q.Encode()
		}
		return next.RoundTrip(r)
	})
}

// NewHTTPCacheGenNumberHeaderSetterMiddleware returns a middleware that sets cache gen header to let consumer of response
// know all previous responses could be invalid due to delete operation.
func NewHTTPCacheGenNumberHeaderSetterMiddleware(cacheGenNumbersLoader CacheGenNumberLoader) middleware.Interface {
	return middleware.Func(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenantIDs, err := tenant.TenantIDs(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}

			cacheGenNumber := cacheGenNumbersLoader.GetResultsCacheGenNumber(tenantIDs)

			w.Header().Set(resultsCacheGenNumberHeaderName, cacheGenNumber)
			next.ServeHTTP(w, r)
		})
	})
}
