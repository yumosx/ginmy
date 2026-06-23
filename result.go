package ginmy

type Result struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Value   any    `json:"value"`
}
