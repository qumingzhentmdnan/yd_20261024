// Package frontend 嵌入不依赖外部 CDN 的本地产品界面。
package frontend

import "embed"

// Files 是随 Host 二进制分发的静态页面。
//
//go:embed web/*
var Files embed.FS
