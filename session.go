package ginmy

// Session 是由调用方实现的会话抽象，用于在业务处理函数（例如 BS）中
// 访问当前请求的认证信息。框架不感知具体实现：JWT、Redis Session、
// Opaque Token 皆可。
//
// Session 接收请求的 token 字符串并返回用户/会话信息。由于 Session
// 在路由注册时通过闭包捕获并在所有请求间共享，Session 应当不持有
// 单个请求的私有数据；其内部状态仅用于共享配置（如密钥、连接池等），
// 且实现必须保证自身在并发调用下的安全。
type Session interface {
	// UID 从 token 中解析并返回用户唯一标识；token 为空或无效时返回空字符串。
	UID(token string) string

	// Username 从 token 中解析并返回用户名；token 为空或无效时返回空字符串。
	Username(token string) string

	// Get 从 token 中解析并返回指定 key 的扩展属性（例如角色、租户 ID、设备指纹）。
	// 第二个返回值表示该属性是否存在，便于业务侧区分零值与未设置。
	Get(token, key string) (any, bool)
}
