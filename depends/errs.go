package depends

import (
	"fmt"
	"reflect"
	"strings"
)

// NotFoundError 表示容器中没有注册某个 (类型, 名字) 对应的工厂。
// 可以用 errors.As 捕获。
type NotFoundError struct {
	Type reflect.Type
	Name string
}

func (e *NotFoundError) Error() string {
	if e.Name == "" {
		return fmt.Sprintf("depends: no factory registered for type %v", e.Type)
	}
	return fmt.Sprintf("depends: no factory registered for type %v with name %q", e.Type, e.Name)
}

// CircularError 表示解析过程中出现了循环依赖（A -> B -> A）。
type CircularError struct {
	Chain []string // 参与循环的类型（含名字）描述
}

func (e *CircularError) Error() string {
	return fmt.Sprintf("depends: circular dependency detected: %s",
		strings.Join(e.Chain, " -> "))
}
