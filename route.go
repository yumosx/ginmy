package ginmy

import "github.com/gin-gonic/gin"

type Router interface {
	Route(engine *gin.Engine)
}
