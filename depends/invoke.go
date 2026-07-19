package depends

import (
	"fmt"
	"reflect"
)

// Invoke 用反射调用 fn，并按 fn 的参数类型依次从 c 中解析后注入。
//
// 返回值是 fn 的所有返回值（已装箱成 any）。
//
// 适合「只想用一次、懒得注册」的临时调用；常规依赖建议用 D。
func Invoke(c *Container, fn any) ([]any, error) {
	if c == nil {
		return nil, fmt.Errorf("depends.Invoke: container is nil")
	}
	if fn == nil {
		return nil, fmt.Errorf("depends.Invoke: fn is nil")
	}
	fnVal := reflect.ValueOf(fn)
	fnType := fnVal.Type()
	if fnType.Kind() != reflect.Func {
		return nil, fmt.Errorf("depends.Invoke: expected function, got %T", fn)
	}

	args, err := c.resolveArgs(fnType, &resolveCtx{})
	if err != nil {
		return nil, err
	}
	out := fnVal.Call(args)
	results := make([]any, len(out))
	for i, r := range out {
		results[i] = r.Interface()
	}
	return results, nil
}

// Call 是 Invoke 的便捷封装：当 fn 只返回一个值时直接拿回该值。
// fn 可以返回 (T) 或 (T, error)，后者在出错时会被透传出来。
func Call[T any](c *Container, fn any) (T, error) {
	results, err := Invoke(c, fn)
	if err != nil {
		var zero T
		return zero, err
	}
	if len(results) == 0 {
		var zero T
		return zero, fmt.Errorf("depends.Call: function returned no values")
	}
	switch len(results) {
	case 1:
		v, ok := results[0].(T)
		if !ok {
			var zero T
			return zero, fmt.Errorf("depends.Call: return type %T is not %T", results[0], zero)
		}
		return v, nil
	case 2:
		v, ok := results[0].(T)
		if !ok {
			var zero T
			return zero, fmt.Errorf("depends.Call: return type %T is not %T", results[0], zero)
		}
		if e, _ := results[1].(error); e != nil {
			return v, e
		}
		return v, nil
	default:
		var zero T
		return zero, fmt.Errorf("depends.Call: unsupported number of return values: %d", len(results))
	}
}
