package ginmy

type Result struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Value   any    `json:"value"`
}

func ErrNoResult(err error) (Result, error) {
	return Result{}, err
}
