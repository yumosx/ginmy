package depends

// Lifetime 决定一个依赖被缓存的粒度。
type Lifetime int

const (
	// LifetimeSingleton（默认）：容器范围内只构造一次。
	LifetimeSingleton Lifetime = iota
	// LifetimeTransient：每次解析都重新构造，不缓存。
	LifetimeTransient
	// LifetimeScoped：在同一个 *Scope 内单例；不同 Scope 各自独立。
	LifetimeScoped
)

// DepOption 修改 Depends 注册时的行为。
type DepOption func(*depConfig)

type depConfig struct {
	name     string
	lifetime Lifetime
}

// Named 给当前依赖打一个名字。同一个类型下可以用不同 name 注册多个实例。
// 解析时通过 ResolveNamed[T](c, "name") 或带名注入取出。
func Named(name string) DepOption {
	return func(c *depConfig) { c.name = name }
}

// Transient 把当前依赖标记为「每次解析都新建」。不写入任何缓存。
func Transient() DepOption {
	return func(c *depConfig) { c.lifetime = LifetimeTransient }
}

// Scoped 把当前依赖标记为「作用域内单例」。
// 通过 NewScope() 创建作用域，并用 dep.GetIn(scope) 解析。
// 若不传 scope 解析，则退化为 Singleton（容器默认作用域）。
func Scoped() DepOption {
	return func(c *depConfig) { c.lifetime = LifetimeScoped }
}
