package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// cgroupRoot 是 cgroup v2 的挂载点；测试里会换成临时目录。
var cgroupRoot = "/sys/fs/cgroup"

// countPodmanAt 数 root 下正在运行的 podman 容器数量，第二个返回值表示是否数得出来。
// root 用户运行的 podman 容器，每个运行中的容器在 machine.slice 下有一个 libpod-<容器ID>.scope 目录。
// 这个目录树所有用户都能读，所以 Agent 不需要 root 权限，也不需要调用 podman 命令。
//   - 只数 machine.slice 的直接子目录：容器里再跑 podman 产生的嵌套目录在容器自己的 cgroup 里，不数。
//   - libpod-conmon-<ID>.scope 是容器的监控进程，不算；已停止的容器没有这个目录，所以只数运行中的。
//   - 不是 cgroup v2（没有 cgroup.controllers）或者读取失败：返回 false，调用方不上报，而不是上报 0。
//   - 没有 machine.slice（没装 podman，或者一个容器都没有跑过）：数量为 0。
//   - 普通用户（rootless）运行的容器在 user.slice 下，不在统计范围内。
func countPodmanAt(root string) (int, bool) {
	if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err != nil {
		return 0, false
	}
	entries, err := os.ReadDir(filepath.Join(root, "machine.slice"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, true
		}
		return 0, false
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() && strings.HasPrefix(name, "libpod-") && !strings.HasPrefix(name, "libpod-conmon-") && strings.HasSuffix(name, ".scope") {
			n++
		}
	}
	return n, true
}
