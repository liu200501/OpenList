package fs

// 本文件定义一个"服务端秒转存"钩子，用于让特定驱动对（如中国移动云盘 139）
// 在跨存储复制时走云端服务端复制（零字节下载），而不是默认的 Link→Put 字节流。
//
// internal/fs 不能 import 任何具体驱动（drivers/crypt 等已 import fs，会循环），
// 因此这里只定义函数类型与注册入口；具体实现放在 internal/transfer139 包，
// 由其 init() 注册进来。
//
// 语义：
//   - hook 做同步的"条件判断 + 轻量预检"，不执行耗时的转存本身。
//   - 命中时返回 run：真正执行转存的闭包（可能耗时数分钟），由 fs 包包装成
//     后台任务异步执行，返回给前端展示；run 的 error 会被记录到任务里。
//   - handled=false 时 transfer() 原样继续走既有复制逻辑（无缝回退）。

import (
	"context"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
)

// ServerCopyRunner 执行一次服务端转存；progress 用于上报进度（0-100），可为 nil。
type ServerCopyRunner func(ctx context.Context, progress func(percent float64)) error

// ServerCopyHook 判断能否用服务端秒转存接管一次跨存储复制。
//
// 参数：
//   ctx:        调用上下文
//   taskType:   "copy" / "move" / "merge"
//   srcObjPath, dstDirPath, dstName: 源对象虚拟路径、目标目录虚拟路径、目标改名
//   srcStorage/srcObjActualPath: 已解析的源存储与实际路径
//   dstStorage/dstDirActualPath: 已解析的目标存储与实际路径
//
// 返回：
//   handled: 是否接管（false=回退到字节流）
//   run:     handled=true 时，真正执行转存的闭包
//   reason:  供任务显示的描述（如 "139 跨账号秒转存"）
type ServerCopyHook func(
	ctx context.Context,
	taskType string,
	srcObjPath, dstDirPath, dstName string,
	srcStorage driver.Driver, srcObjActualPath string,
	dstStorage driver.Driver, dstDirActualPath string,
) (handled bool, run ServerCopyRunner, reason string)

var serverCopyHook ServerCopyHook

// RegisterServerCopyHook 注册服务端秒转存钩子（由 internal/transfer139 的 init 调用）。
func RegisterServerCopyHook(h ServerCopyHook) {
	serverCopyHook = h
}
