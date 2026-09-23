// Package web 通过 go:embed 将前端静态资源内嵌进二进制。
//
// 这样最终产物为单一可执行文件，无需额外部署 HTML/CSS/JS。
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

// staticFS 内嵌 static 目录下的全部文件（index.html、css、js 等）。
//
//go:embed static
var staticFS embed.FS

// FileServer 返回一个用于服务内嵌静态资源的 http.Handler。
//
// 它将 static 子目录作为根目录对外提供，因此访问 "/" 即对应 static/index.html。
func FileServer() http.Handler {
	// 去掉 "static" 前缀，使 URL 路径直接对应目录内文件。
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// 内嵌目录固定存在，理论上不会触发；触发即为编译期资源缺失。
		panic("web: 无法定位内嵌 static 目录: " + err.Error())
	}
	return http.FileServer(http.FS(sub))
}

// FS 返回内嵌静态资源的只读文件系统，便于测试或自定义服务逻辑。
func FS() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic("web: 无法定位内嵌 static 目录: " + err.Error())
	}
	return sub
}
