// Package web 把控制台静态文件打进二进制，由 API 服务同源托管。
package web

import "embed"

//go:embed index.html css js
var FS embed.FS
