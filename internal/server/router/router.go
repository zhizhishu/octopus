package router

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// GroupRouter represents a group of routes with shared path prefix and middlewares
type GroupRouter struct {
	Path        string
	Routes      []*Route
	Middlewares []gin.HandlerFunc
}

// Global registry for route groups
var registeredRouters []*GroupRouter

// NewGroupRouter creates a new GroupRouter with the given path and automatically registers it.
func NewGroupRouter(path string) *GroupRouter {
	router := &GroupRouter{
		Path:   path,
		Routes: make([]*Route, 0),
	}
	registeredRouters = append(registeredRouters, router)
	return router
}

// Use adds middlewares to the group.
func (g *GroupRouter) Use(middlewares ...gin.HandlerFunc) *GroupRouter {
	g.Middlewares = append(g.Middlewares, middlewares...)
	return g
}

// AddRoute adds a route to the group.
func (g *GroupRouter) AddRoute(route *Route) *GroupRouter {
	g.Routes = append(g.Routes, route)
	return g
}

// Route defines a single endpoint with its handlers and middlewares.
type Route struct {
	Path        string
	Method      string
	Handlers    []gin.HandlerFunc
	Middlewares []gin.HandlerFunc
}

// NewRoute creates a new Route instance with the given path and method.
func NewRoute(path string, method string) *Route {
	return &Route{
		Path:     path,
		Method:   method,
		Handlers: make([]gin.HandlerFunc, 0),
	}
}

// Handle adds handler functions to the route.
func (r *Route) Handle(handlers ...gin.HandlerFunc) *Route {
	r.Handlers = append(r.Handlers, handlers...)
	return r
}

// Use adds middlewares to the route.
func (r *Route) Use(middlewares ...gin.HandlerFunc) *Route {
	r.Middlewares = append(r.Middlewares, middlewares...)
	return r
}

// Validate checks if the route is valid
func (r *Route) Validate() error {
	if len(r.Handlers) == 0 {
		return fmt.Errorf("route must have at least one handler")
	}
	return nil
}

// GetRouterCount returns the total count of registered routes
func GetRouterCount() int {
	count := 0
	for _, router := range registeredRouters {
		count += len(router.Routes)
	}
	return count
}

// RegisterAll registers all globally registered route groups to the Gin engine
func RegisterAll(engine *gin.Engine) error {
	for _, router := range registeredRouters {
		// Validate all routes in the group first
		for _, route := range router.Routes {
			if err := route.Validate(); err != nil {
				return fmt.Errorf("invalid route in group %s: %w", router.Path, err)
			}
		}

		// Create the route group
		group := engine.Group(router.Path, router.Middlewares...)

		// Register all routes in the group
		for _, route := range router.Routes {
			handlers := make([]gin.HandlerFunc, 0, len(route.Middlewares)+len(route.Handlers))
			handlers = append(handlers, route.Middlewares...)
			handlers = append(handlers, route.Handlers...)

			registerRoute(group, route.Method, route.Path, handlers)
		}
	}
	// 🔴 这里曾经是 `registeredRouters = nil`（"注册完就清空，防重复注册"）。
	// 但 registeredRouters 是**路由描述表**，不是一次性队列：把同一份描述注册到
	// 两个 engine 上是完全合法且必要的（子引擎、灰度引擎、测试各建一个）。
	// 清空它带来的是纯粹的副作用：
	//   - 同进程第二个 engine 拿到 0 条路由，所有路径 404；
	//   - 单元测试里谁先跑谁拿到路由，后跑的测试无论断言什么都与真实行为无关
	//     （404 既不是 200 也不是 401，会让鉴权测试"假绿"或"假红"）。
	// 重复注册到**同一个** engine 由 gin 自己处理（同 path+method 覆盖），
	// 不需要在这里擦状态。
	return nil
}

// registerRoute registers a single route to a Gin route group.
func registerRoute(group *gin.RouterGroup, method string, path string, handlers []gin.HandlerFunc) {
	if len(handlers) == 0 {
		return
	}

	if path != "" {
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
	}

	switch method {
	case http.MethodGet:
		group.GET(path, handlers...)
	case http.MethodPost:
		group.POST(path, handlers...)
	case http.MethodPut:
		group.PUT(path, handlers...)
	case http.MethodDelete:
		group.DELETE(path, handlers...)
	case http.MethodHead:
		group.HEAD(path, handlers...)
	case http.MethodOptions:
		group.OPTIONS(path, handlers...)
	case http.MethodPatch:
		group.PATCH(path, handlers...)
	default:
		group.GET(path, handlers...)
	}
}
