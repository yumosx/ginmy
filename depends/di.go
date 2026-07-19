// Package depends 提供一个轻量的依赖注入容器：按类型注册工厂，解析时自动
// 注入形参，并按 Lifetime（单例 / 每次新建 / 作用域内单例）缓存结果。
//
// 支持同类型多实例（通过 depends.Named("name") 区分）、循环依赖检测以及
// 注册期的浅校验。
package depends

import (
	"fmt"
	"reflect"
	"sync"
)

// -----------------------------------------------------------------------------
// 内部：缓存 key
// -----------------------------------------------------------------------------

// key 是 (类型, 名字) 二元组，用作 factory / cache / inflight 的索引。
type key struct {
	t    reflect.Type
	name string
}

func (k key) String() string {
	if k.name == "" {
		return fmt.Sprintf("%v", k.t)
	}
	return fmt.Sprintf("%v[name=%q]", k.t, k.name)
}

// -----------------------------------------------------------------------------
// Container / Scope
// -----------------------------------------------------------------------------

// Container 持有「key -> 工厂」的映射，并按 Lifetime 缓存已解析的实例。
type Container struct {
	mu             sync.RWMutex
	factories      map[key]entry
	cache          map[key]any // 容器级缓存（Singleton + 无显式 scope 的 Scoped）
	inflight       map[key]chan struct{}
	nextScope      uint64
	scopes         map[uint64]*Scope
	strictValidate bool // true 时 D() 注册期会校验形参是否已有 provider
	validateWarned bool // 严格校验缺失时只 warn 一次
}

type entry struct {
	factory  func(c *Container, ctx *resolveCtx) (any, error)
	lifetime Lifetime
}

// resolveCtx 在解析调用链中传递，用于：
//   - 检测循环依赖
//   - 把当前 scope 传给子依赖（Scoped 行为）
type resolveCtx struct {
	chain []key
	scope *Scope
}

// New 创建一个空的容器。
func New() *Container {
	return &Container{
		factories: make(map[key]entry),
		cache:     make(map[key]any),
		inflight:  make(map[key]chan struct{}),
		scopes:    make(map[uint64]*Scope),
	}
}

// EnableStrictValidation 开启注册期形参校验：调用 D() 时若 fn 的某个形参
// 类型在容器内没有 provider，立即 panic。这样能在启动期把缺依赖的错误
// 提前暴露出来；关闭时（默认）保持惰性解析、错误延迟到 Get() 时。
func (c *Container) EnableStrictValidation() {
	c.mu.Lock()
	c.strictValidate = true
	c.mu.Unlock()
}

// NewScope 创建一个独立的作用域。Scoped 依赖在同一个 scope 内是单例的，
// 不同 scope 之间互不影响。返回的 scope 需在不再使用时调用 Close() 释放。
func (c *Container) NewScope() *Scope {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextScope++
	s := &Scope{
		id:    c.nextScope,
		c:     c,
		cache: make(map[key]any),
	}
	c.scopes[c.nextScope] = s
	return s
}

// Scope 是一个独立的作用域，Scoped 依赖在 scope 内单例。
type Scope struct {
	id    uint64
	c     *Container
	mu    sync.RWMutex
	cache map[key]any
}

// Close 释放该 scope（不再持有引用）。不会影响 Singleton 缓存。
func (s *Scope) Close() {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	delete(s.c.scopes, s.id)
	s.mu.Lock()
	s.cache = nil
	s.mu.Unlock()
}

// ResolveIn Resolve 在该 scope 内解析一个依赖。
func ResolveIn[T any](scope *Scope, dep *Dep[T]) (T, error) {
	if dep == nil {
		var zero T
		return zero, fmt.Errorf("depends.ResolveIn: dep is nil")
	}
	return dep.getIn(scope)
}

// MustResolveIn 是 ResolveIn 的 panic 版本。
func MustResolveIn[T any](scope *Scope, dep *Dep[T]) T {
	v, err := ResolveIn(scope, dep)
	if err != nil {
		panic(err)
	}
	return v
}

// -----------------------------------------------------------------------------
// 内部：解析核心
// -----------------------------------------------------------------------------

// resolve 是统一的解析入口，按 lifetime 走不同的缓存路径。
func (c *Container) resolve(k key, lifetime Lifetime, scope *Scope, ctx *resolveCtx) (any, error) {
	// 1) 循环依赖检测：链上已出现过相同 key。
	for _, prev := range ctx.chain {
		if prev == k {
			chain := make([]string, 0, len(ctx.chain)+1)
			for _, p := range ctx.chain {
				chain = append(chain, p.String())
			}
			chain = append(chain, k.String())
			return nil, &CircularError{Chain: chain}
		}
	}
	newCtx := &resolveCtx{
		chain: append(append([]key(nil), ctx.chain...), k),
		scope: scope,
	}

	// 2) Transient：每次都重新构造，不读不写缓存。
	if lifetime == LifetimeTransient {
		return c.invokeFactory(k, newCtx)
	}

	// 3) 决定缓存表。
	var cacheMu *sync.RWMutex
	var cache map[key]any
	if lifetime == LifetimeScoped {
		// 没传 scope：退化到容器级缓存，行为退化为 Singleton。
		if scope == nil {
			cacheMu = &c.mu
			cache = c.cache
		} else {
			cacheMu = &scope.mu
			cache = scope.cache
		}
	} else {
		cacheMu = &c.mu
		cache = c.cache
	}

	// 4) 快速路径：缓存命中。
	cacheMu.RLock()
	if v, ok := cache[k]; ok {
		cacheMu.RUnlock()
		return v, nil
	}
	cacheMu.RUnlock()

	// 5) 慢速路径：拿写锁争抢构造权 / 等待别人。
	cacheMu.Lock()
	if v, ok := cache[k]; ok { // double-check
		cacheMu.Unlock()
		return v, nil
	}
	if ch, ok := c.inflight[k]; ok {
		cacheMu.Unlock()
		<-ch
		// 别人构造完了，再检查一次缓存。
		cacheMu.RLock()
		v, ok := cache[k]
		cacheMu.RUnlock()
		if ok {
			return v, nil
		}
		return nil, fmt.Errorf("depends: %v resolved concurrently but not cached", k)
	}
	ch := make(chan struct{})
	c.inflight[k] = ch
	cacheMu.Unlock()

	// 6) 真正调用工厂（持锁外执行，避免递归死锁）。
	v, err := c.invokeFactory(k, newCtx)

	// 7) 收尾：清理 inflight，按结果写缓存。
	cacheMu.Lock()
	delete(c.inflight, k)
	close(ch)
	if err != nil {
		cacheMu.Unlock()
		return nil, err
	}
	if v == nil {
		cacheMu.Unlock()
		return nil, fmt.Errorf("depends: factory for %v returned nil", k)
	}
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Ptr && rv.IsNil() {
		cacheMu.Unlock()
		return nil, fmt.Errorf("depends: factory for %v returned typed nil", k)
	}
	if existing, ok := cache[k]; ok {
		// 极端并发：已被别人先缓存了，沿用旧值。
		cacheMu.Unlock()
		return existing, nil
	}
	cache[k] = v
	cacheMu.Unlock()
	return v, nil
}

func (c *Container) invokeFactory(k key, ctx *resolveCtx) (any, error) {
	c.mu.RLock()
	e, ok := c.factories[k]
	c.mu.RUnlock()
	if !ok {
		return nil, &NotFoundError{Type: k.t, Name: k.name}
	}
	return e.factory(c, ctx)
}

// resolveArgs 按 fnType 的形参列表，依次从容器中解析每个参数。
// 参数的 scope 与当前解析链一致；参数自身的 lifetime 决定缓存策略。
func (c *Container) resolveArgs(fnType reflect.Type, ctx *resolveCtx) ([]reflect.Value, error) {
	args := make([]reflect.Value, fnType.NumIn())
	for i := 0; i < fnType.NumIn(); i++ {
		paramType := fnType.In(i)
		// 形参按「无名」key 解析：如果用户注册了同名 variant，这里只能取无名那一个。
		// 想注入带名 variant，请使用 ResolveNamed + Dep.Named 显式查找。
		k := key{t: paramType, name: ""}
		c.mu.RLock()
		e, ok := c.factories[k]
		c.mu.RUnlock()
		if !ok {
			// 兜底：如果有且仅有一个带名 variant，也允许解析（避免多 DB 这种场景下崩）
			if alt, altOK := c.findSoleNamedVariant(paramType); altOK {
				k = alt
				c.mu.RLock()
				e, ok = c.factories[k]
				c.mu.RUnlock()
			}
		}
		if !ok {
			return nil, fmt.Errorf("depends: parameter %d (%v): %w",
				i, paramType, &NotFoundError{Type: paramType})
		}
		v, err := c.resolve(k, e.lifetime, ctx.scope, ctx)
		if err != nil {
			return nil, fmt.Errorf("depends: parameter %d (%v): %w", i, paramType, err)
		}
		args[i] = reflect.ValueOf(v)
	}
	return args, nil
}

// findSoleNamedVariant 在类型 t 下若只存在唯一一个带名 variant，返回它的 key。
// 用于让形参自动解析唯一的带名依赖。
func (c *Container) findSoleNamedVariant(t reflect.Type) (key, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var found key
	count := 0
	for k := range c.factories {
		if k.t == t && k.name != "" {
			found = k
			count++
		}
	}
	if count == 1 {
		return found, true
	}
	return key{}, false
}

// register 把 (key -> entry) 写入工厂表。重复注册会 panic。
func (c *Container) register(k key, e entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.factories[k]; exists {
		panic(fmt.Sprintf("depends: factory for %v already registered", k))
	}
	c.factories[k] = e
}

// hasFactory 供 Depends 注册期校验使用（仅查无名 key）。
func (c *Container) hasFactory(t reflect.Type, name string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.factories[key{t: t, name: name}]
	return ok
}

// -----------------------------------------------------------------------------
// Depends / Dep
// -----------------------------------------------------------------------------

// Dep 是一个可解析的依赖句柄。通过 D() 拿到，调用 Get/MustGet 即可拿到值。
type Dep[T any] struct {
	c        *Container
	k        key
	lifetime Lifetime
}

// Get 解析该依赖并返回值；解析过程会按需触发整条依赖链。
func (d *Dep[T]) Get() (T, error) { return d.getIn(nil) }

// MustGet 是 Get 的 panic 版本。
func (d *Dep[T]) MustGet() T {
	v, err := d.Get()
	if err != nil {
		panic(err)
	}
	return v
}

// GetIn 在指定 scope 内解析该依赖（仅对 Scoped 有意义）。
func (d *Dep[T]) GetIn(scope *Scope) (T, error) { return d.getIn(scope) }

// MustGetIn 是 GetIn 的 panic 版本。
func (d *Dep[T]) MustGetIn(scope *Scope) T {
	v, err := d.GetIn(scope)
	if err != nil {
		panic(err)
	}
	return v
}

func (d *Dep[T]) getIn(scope *Scope) (T, error) {
	var zero T
	v, err := d.c.resolve(d.k, d.lifetime, scope, &resolveCtx{})
	if err != nil {
		return zero, err
	}
	out, ok := v.(T)
	if !ok {
		return zero, fmt.Errorf("depends: cached value %T cannot be asserted to %v", v, d.k.t)
	}
	return out, nil
}

// Container 返回该依赖所属的容器。
func (d *Dep[T]) Container() *Container { return d.c }

// Lifetime 返回该依赖的缓存策略。
func (d *Dep[T]) Lifetime() Lifetime { return d.lifetime }

// Name 返回该依赖注册时的名字。
func (d *Dep[T]) Name() string { return d.k.name }

// D 把工厂 fn 注册为类型 T 的提供者，并返回一个 *Dep[T] 句柄。
//
// fn 的参数类型会自动从 c 中解析（顺序无关、解析是惰性的）。
// fn 的第一个返回值必须可断言为 T，否则会 panic。fn 的返回值支持：
//   - 仅 T
//   - (T, error)
//
// 可选 opts：
//   - depends.Named("name")：让同名/同类型多实例可区分
//   - depends.Transient()：每次解析都新建
//   - depends.Scoped()：在 *Scope 内单例
func D[T any](c *Container, fn any, opts ...DepOption) *Dep[T] {
	if c == nil {
		panic("depends.D: container is nil")
	}
	if fn == nil {
		panic("depends.D: fn is nil")
	}

	cfg := depConfig{lifetime: LifetimeSingleton}
	for _, opt := range opts {
		opt(&cfg)
	}

	// 推断 T：T 是接口类型时，var zero T 是无类型 nil interface，
	// reflect.TypeOf(zero) 会返回 nil；用 *T 指针类型反射 .Elem() 才能拿到接口类型本身。
	var zero T
	t := reflect.TypeOf(zero)
	if t == nil {
		t = reflect.TypeOf(&zero).Elem()
	}
	if t == nil {
		panic(fmt.Sprintf("depends.D: cannot infer type for %T", zero))
	}

	fnVal := reflect.ValueOf(fn)
	fnType := fnVal.Type()
	if fnType.Kind() != reflect.Func {
		panic(fmt.Sprintf("depends.D: fn must be a function, got %T", fn))
	}

	k := key{t: t, name: cfg.name}

	// 注册期浅校验（可选）：fn 的形参对应的「无名」工厂必须已经存在（如果有的话）。
	// 仅在显式调用 c.EnableStrictValidation() 后才生效。
	if c.strictValidate {
		for i := 0; i < fnType.NumIn(); i++ {
			pt := fnType.In(i)
			if !c.hasFactory(pt, "") && !c.hasAnyNamedFactory(pt) {
				panic(fmt.Sprintf(
					"depends.D: parameter %d of factory for %v has no registered provider (type %v)",
					i, k, pt,
				))
			}
		}
	}

	factory := func(c *Container, ctx *resolveCtx) (any, error) {
		resolved, err := c.resolveArgs(fnType, ctx)
		if err != nil {
			return nil, err
		}
		out := fnVal.Call(resolved)
		return interpretResults[T](t, out)
	}

	c.register(k, entry{factory: factory, lifetime: cfg.lifetime})
	return &Dep[T]{c: c, k: k, lifetime: cfg.lifetime}
}

// hasAnyNamedFactory 判断类型 t 下是否存在任意（带名或不带名）工厂。
func (c *Container) hasAnyNamedFactory(t reflect.Type) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for k := range c.factories {
		if k.t == t {
			return true
		}
	}
	return false
}

// interpretResults 把反射调用结果归一为 (T, error)。
func interpretResults[T any](t reflect.Type, out []reflect.Value) (any, error) {
	var zero T
	switch len(out) {
	case 0:
		return zero, nil
	case 1:
		v, ok := out[0].Interface().(T)
		if !ok {
			return nil, fmt.Errorf(
				"depends.D[%v]: factory returned %v, cannot assert to %v",
				t, out[0].Type(), t,
			)
		}
		return v, nil
	case 2:
		if e, _ := out[1].Interface().(error); e != nil {
			return nil, e
		}
		v, ok := out[0].Interface().(T)
		if !ok {
			return nil, fmt.Errorf(
				"depends.D[%v]: factory returned %v, cannot assert to %v",
				t, out[0].Type(), t,
			)
		}
		return v, nil
	default:
		return nil, fmt.Errorf("depends.D[%v]: unsupported number of return values: %d", t, len(out))
	}
}

// -----------------------------------------------------------------------------
// 便捷全局解析（不返回 Dep 时使用）
// -----------------------------------------------------------------------------

// Resolve 按类型 T 取一个实例（走无名 key）。
func Resolve[T any](c *Container) (T, error) {
	return ResolveNamed[T](c, "")
}

// MustResolve 是 Resolve 的 panic 版本。
func MustResolve[T any](c *Container) T {
	v, err := Resolve[T](c)
	if err != nil {
		panic(err)
	}
	return v
}

// ResolveNamed 按 (类型 T, 名字 name) 取一个实例。
func ResolveNamed[T any](c *Container, name string) (T, error) {
	var zero T
	t := reflect.TypeOf(zero)
	if t == nil {
		t = reflect.TypeOf(&zero).Elem()
	}
	if t == nil {
		return zero, fmt.Errorf("depends.ResolveNamed: cannot infer type for %T", zero)
	}
	return resolveNamed[T](c, t, name, nil)
}

// MustResolveNamed 是 ResolveNamed 的 panic 版本。
func MustResolveNamed[T any](c *Container, name string) T {
	v, err := ResolveNamed[T](c, name)
	if err != nil {
		panic(err)
	}
	return v
}

// ResolveInNamed 在指定 scope 内按 (类型, 名字) 解析。
func ResolveInNamed[T any](scope *Scope, name string) (T, error) {
	if scope == nil {
		var zero T
		return zero, fmt.Errorf("depends.ResolveInNamed: scope is nil")
	}
	var zero T
	t := reflect.TypeOf(zero)
	if t == nil {
		t = reflect.TypeOf(&zero).Elem()
	}
	if t == nil {
		return zero, fmt.Errorf("depends.ResolveInNamed: cannot infer type for %T", zero)
	}
	return resolveNamed[T](scope.c, t, name, scope)
}

func resolveNamed[T any](c *Container, t reflect.Type, name string, scope *Scope) (T, error) {
	var zero T
	k := key{t: t, name: name}

	// 先查 lifetime：带名注册的也是 Singleton 默认。
	c.mu.RLock()
	e, ok := c.factories[k]
	c.mu.RUnlock()
	if !ok {
		return zero, &NotFoundError{Type: t, Name: name}
	}
	v, err := c.resolve(k, e.lifetime, scope, &resolveCtx{})
	if err != nil {
		return zero, err
	}
	out, ok := v.(T)
	if !ok {
		return zero, fmt.Errorf("depends.Resolve: cached value %T cannot be asserted to %v", v, t)
	}
	return out, nil
}
