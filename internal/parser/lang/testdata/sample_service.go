// Package orderservice implements a sample domain service used as a 500+ line
// Phase 7 golden fixture for verifying Go AST symbol, import, type, variable,
// and call-site extraction.
package orderservice

import (
	localcfg "./internal/cfg"
	"context"
	_ "embed"
	"errors"
	"fmt"
	syncalias "sync"
	"time"
	. "unicode"
)

// ServiceVersion identifies the fixture API revision.
const ServiceVersion = "2.4.0"

const (
	// StatusPending marks newly created orders.
	StatusPending StatusCode = iota + 1
	StatusProcessing
	StatusCompleted
	StatusFailed
	internalRetryLimit = 3
)

// ErrOrderNotFound indicates a missing order lookup.
var ErrOrderNotFound = errors.New("order not found")

var (
	// DefaultTimeout bounds upstream storage calls.
	DefaultTimeout       time.Duration = 5 * time.Second
	fallbackRegion, zone string        = "us-east-1", "a"
	globalRegistry                     = NewRegistry("default")
)

// OrderID aliases string identifiers for orders.
type OrderID = string

// GenericIndex aliases a typed lookup map.
type GenericIndex[V any] = map[OrderID]V

// StatusCode represents numeric lifecycle states.
type StatusCode int

// Middleware wraps handler execution.
type Middleware func(next HandlerFunc) HandlerFunc

// HandlerFunc processes a single order command.
type HandlerFunc func(ctx context.Context, cmd Command) error

// Identifier constrains supported generic keys.
type Identifier interface {
	~string | ~int64
}

// ReaderStore defines persistence read operations.
type ReaderStore interface {
	Get(ctx context.Context, id OrderID) (*Order, error)
	ListByCustomer(ctx context.Context, customerID string, limit int) ([]*Order, error)
}

// WriterStore defines persistence write operations.
type WriterStore interface {
	Save(ctx context.Context, order *Order) error
	Delete(ctx context.Context, id OrderID) error
}

// Repository composes read and write store capabilities.
type Repository interface {
	ReaderStore
	WriterStore
	Ping(ctx context.Context) error
}

// AuditLogger records structured audit events.
type AuditLogger interface {
	Record(event string, attrs map[string]any)
}

// BaseEntity supplies shared timestamps and identifier fields.
type BaseEntity struct {
	// ID uniquely identifies the entity.
	ID                   string `json:"id"`
	CreatedAt, UpdatedAt int64
	deleted              bool
}

// Cache holds bounded in-memory entries.
type Cache[K Identifier, V any] struct {
	mu      syncalias.RWMutex
	entries map[K]V
	maxSize int
}

// Command carries an incoming mutation payload.
type Command struct {
	Kind      string `json:"kind"`
	OrderID   OrderID
	Payload   []byte
	Attempt   int
	DryRun    bool
	Requester string
}

// Order represents a persisted customer order.
type Order struct {
	BaseEntity
	CustomerID string   `json:"customer_id"`
	Items      []string `json:"items"`
	TotalCents int64    `json:"total_cents"`
	Status     StatusCode
	Notes      map[string]string
}

// OrderRegistry tracks named service instances.
type OrderRegistry struct {
	name    string
	mu      syncalias.Mutex
	aliases GenericIndex[string]
}

// Service coordinates order validation, caching, persistence, and auditing.
type Service[T any] struct {
	BaseEntity
	*syncalias.RWMutex
	repo        Repository
	logger      AuditLogger
	cache       *Cache[OrderID, *Order]
	middlewares []Middleware
	extra       T
	closed      bool
}

// MetricsSnapshot summarizes execution counters.
type MetricsSnapshot struct {
	ProcessedCount uint64
	ErrorCount     uint64
	ActiveWorkers  int
}

func init() {
	registerBuiltinCodecs()
}

func init() {
	localcfg.EnsureDefaults()
}

func registerBuiltinCodecs() {
	_ = IsLetter('A')
}

// NewRegistry initializes a named OrderRegistry.
func NewRegistry(name string) *OrderRegistry {
	return &OrderRegistry{
		name:    name,
		aliases: make(GenericIndex[string]),
	}
}

// NewCache creates a Cache sized to capacity.
func NewCache[K Identifier, V any](capacity int) *Cache[K, V] {
	if capacity <= 0 {
		capacity = 16
	}
	return &Cache[K, V]{
		entries: make(map[K]V, capacity),
		maxSize: capacity,
	}
}

// Get retrieves an item by key if present.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	val, ok := c.entries[key]
	return val, ok
}

// Set inserts or replaces a cached entry.
func (c *Cache[K, V]) Set(key K, val V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.maxSize {
		c.evictOne()
	}
	c.entries[key] = val
}

func (c *Cache[K, V]) evictOne() {
	for k := range c.entries {
		delete(c.entries, k)
		return
	}
}

// Clone duplicates an Order defensively.
func (o *Order) Clone() *Order {
	if o == nil {
		return nil
	}
	itemsCopy := make([]string, len(o.Items))
	copy(itemsCopy, o.Items)
	notesCopy := make(map[string]string, len(o.Notes))
	for k, v := range o.Notes {
		notesCopy[k] = v
	}
	return &Order{
		BaseEntity: o.BaseEntity,
		CustomerID: o.CustomerID,
		Items:      itemsCopy,
		TotalCents: o.TotalCents,
		Status:     o.Status,
		Notes:      notesCopy,
	}
}

// NewService constructs a configured Service instance.
func NewService[T any](repo Repository, logger AuditLogger, extra T, middlewares ...Middleware) *Service[T] {
	srv := &Service[T]{
		BaseEntity: BaseEntity{
			ID:        "svc-main",
			CreatedAt: time.Now().Unix(),
		},
		RWMutex:     &syncalias.RWMutex{},
		repo:        repo,
		logger:      logger,
		cache:       NewCache[OrderID, *Order](128),
		middlewares: middlewares,
		extra:       extra,
	}
	return srv
}

// FetchOrder loads an order from cache or backing repository.
func (s *Service[T]) FetchOrder(ctx context.Context, id OrderID) (*Order, error) {
	if id == "" || s.closed {
		return nil, ErrOrderNotFound
	}
	if cached, ok := s.cache.Get(id); ok {
		return cached.Clone(), nil
	}
	order, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("fetch order %s: %w", id, err)
	}
	if order != nil {
		s.cache.Set(id, order.Clone())
	}
	return order, nil
}

// ProcessCommand validates and routes commands with retry and instrumentation.
func (s *Service[T]) ProcessCommand(ctx context.Context, cmd Command) error {
	if err := s.validateCommand(cmd); err != nil {
		s.audit("command_invalid", cmd.OrderID, err)
		return err
	}

	var handler HandlerFunc = func(execCtx context.Context, c Command) error {
		return s.dispatch(execCtx, c)
	}

	for i := len(s.middlewares) - 1; i >= 0; i-- {
		mw := s.middlewares[i]
		if mw != nil {
			handler = mw(handler)
		}
	}

	attempts := 0
	for attempts < internalRetryLimit {
		attempts++
		err := handler(ctx, cmd)
		if err == nil {
			s.audit("command_ok", cmd.OrderID, nil)
			return nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, ErrOrderNotFound) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}

	return fmt.Errorf("command %s exhausted retries", cmd.Kind)
}

func (s *Service[T]) validateCommand(cmd Command) error {
	if cmd.Kind == "" || cmd.OrderID == "" {
		return errors.New("missing kind or order id")
	}
	if cmd.Attempt < 0 && !cmd.DryRun {
		return errors.New("negative attempt count")
	}
	switch cmd.Kind {
	case "create", "update", "cancel":
		if len(cmd.Payload) == 0 && cmd.Kind != "cancel" {
			return errors.New("empty payload")
		}
	case "noop":
		return nil
	default:
		return fmt.Errorf("unsupported command %q", cmd.Kind)
	}
	return nil
}

func (s *Service[T]) dispatch(ctx context.Context, cmd Command) error {
	switch cmd.Kind {
	case "create":
		return s.handleCreate(ctx, cmd)
	case "update":
		return s.handleUpdate(ctx, cmd)
	case "cancel":
		return s.handleCancel(ctx, cmd)
	default:
		return nil
	}
}

func (s *Service[T]) handleCreate(ctx context.Context, cmd Command) error {
	order, err := localcfg.Decode[Order](cmd.Payload)
	if err != nil {
		return err
	}
	order.Status = StatusPending
	order.CreatedAt = time.Now().Unix()
	if err := s.repo.Save(ctx, &order); err != nil {
		return err
	}
	s.cache.Set(order.ID, (&order).Clone())
	return nil
}

func (s *Service[T]) handleUpdate(ctx context.Context, cmd Command) error {
	order, err := s.FetchOrder(ctx, cmd.OrderID)
	if err != nil {
		return err
	}
	order.Status = StatusProcessing
	order.UpdatedAt = time.Now().Unix()
	if err := s.repo.Save(ctx, order); err != nil {
		return err
	}
	s.cache.Set(order.ID, order.Clone())
	return nil
}

func (s *Service[T]) handleCancel(ctx context.Context, cmd Command) error {
	order, err := s.FetchOrder(ctx, cmd.OrderID)
	if err != nil {
		return err
	}
	if order.Status == StatusCompleted || order.Status == StatusFailed {
		return errors.New("cannot cancel terminal order")
	}
	order.Status = StatusFailed
	order.UpdatedAt = time.Now().Unix()
	return s.repo.Save(ctx, order)
}

// RunBatch processes commands concurrently across worker goroutines.
func (s *Service[T]) RunBatch(ctx context.Context, cmds []Command, workers int) MetricsSnapshot {
	if workers <= 0 {
		workers = 1
	}
	jobs := make(chan Command, len(cmds))
	for _, c := range cmds {
		jobs <- c
	}
	close(jobs)

	var wg syncalias.WaitGroup
	var snap MetricsSnapshot
	var mu syncalias.Mutex

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for cmd := range jobs {
				err := s.ProcessCommand(ctx, cmd)
				mu.Lock()
				if err != nil {
					snap.ErrorCount++
				} else {
					snap.ProcessedCount++
				}
				mu.Unlock()
			}
		}()
	}

	func() {
		wg.Wait()
	}()

	snap.ActiveWorkers = 0
	return snap
}

func (s *Service[T]) audit(event string, id OrderID, err error) {
	if s.logger == nil {
		return
	}
	attrs := map[string]any{
		"order_id": id,
	}
	if err != nil {
		attrs["error"] = err.Error()
	}
	s.logger.Record(event, attrs)
}

// Close marks the service closed and flushes background state.
func (s *Service[T]) Close() error {
	s.Lock()
	defer s.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return nil
}

// HealthCheck inspects backing store readiness and runtime status.
func (s *Service[T]) HealthCheck(ctx context.Context) error {
	if s == nil {
		return errors.New("nil service")
	}
	s.RLock()
	closed := s.closed
	s.RUnlock()
	if closed {
		return errors.New("service closed")
	}
	if err := s.repo.Ping(ctx); err != nil {
		return err
	}
	fn := func() string {
		return formatHealthLabel(s.ID, ServiceVersion)
	}
	_ = fn()
	return nil
}

// DescribeValue formats heterogeneous domain metadata using a type switch.
func DescribeValue(v any) string {
	switch val := v.(type) {
	case string:
		return formatHealthLabel("str", val)
	case int, int64:
		return fmt.Sprintf("num:%v", val)
	case *Order:
		if val == nil {
			return "order:nil"
		}
		return formatHealthLabel("order", val.ID)
	case StatusCode:
		switch val {
		case StatusPending:
			return "status:pending"
		case StatusProcessing:
			return "status:processing"
		case StatusCompleted:
			return "status:completed"
		default:
			return "status:other"
		}
	default:
		return "unknown"
	}
}

func formatHealthLabel(prefix, value string) string {
	return prefix + ":" + value
}

// WarmupOrders asynchronously preloads active orders into the cache.
func (s *Service[T]) WarmupOrders(ctx context.Context, customerID string, ids ...string) int {
	loaded := 0
	for _, id := range ids {
		if id == "" {
			continue
		}
		order, err := s.FetchOrder(ctx, id)
		if err != nil || order == nil {
			continue
		}
		if order.CustomerID == customerID {
			loaded++
		}
	}
	if s.logger != nil {
		s.audit("warmup_done", OrderID(customerID), nil)
	}
	return loaded
}

// ListCustomerSummaries fetches order snapshots and projects brief summaries.
func (s *Service[T]) ListCustomerSummaries(ctx context.Context, customerID string, limit int) ([]string, error) {
	if customerID == "" {
		return nil, errors.New("empty customer id")
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	orders, err := s.repo.ListByCustomer(ctx, customerID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(orders))
	for _, o := range orders {
		if o == nil || o.deleted {
			continue
		}
		summary := fmt.Sprintf("%s:%d:%s", o.ID, o.TotalCents, DescribeValue(o.Status))
		out = append(out, summary)
	}
	return out, nil
}

// DrainWithTimeout triggers close after ctx cancellation or timer expiry.
func (s *Service[T]) DrainWithTimeout(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return s.Close()
	case <-timer.C:
		return s.Close()
	}
}

// AuditHook wraps a plain helper for parenthesized invocation coverage.
func AuditHook(logger AuditLogger) {
	if logger == nil {
		return
	}
	fn := logger.Record
	(fn)("hook_ready", map[string]any{"version": ServiceVersion})
}

// ReconcileBatch runs sequential validation across multiple orders and aggregates errors.
func ReconcileBatch(orders []*Order, strict bool) (valid int, errs []error) {
	for _, o := range orders {
		if o == nil {
			if strict {
				errs = append(errs, errors.New("nil order"))
			}
			continue
		}
		if o.ID == "" || o.CustomerID == "" {
			errs = append(errs, fmt.Errorf("invalid order fields"))
			continue
		}
		if o.TotalCents < 0 || len(o.Items) == 0 {
			if strict {
				errs = append(errs, fmt.Errorf("order %s empty", o.ID))
				continue
			}
		}
		valid++
	}
	return valid, errs
}
