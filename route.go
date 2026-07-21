package ginmy

import "github.com/gin-gonic/gin"

type Router interface {
	// route 方法用来注册当前handler 或者 controler 下面所有的 API
	Route(engine *gin.Engine)
}
