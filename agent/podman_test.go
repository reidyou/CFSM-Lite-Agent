package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseConfigPodman(t *testing.T) {
	cur := defaultConfig()
	if cur.Podman {
		t.Fatal("podman must be off by default")
	}
	on, err := parseConfig("report_interval=60&podman=1", cur)
	if err != nil || !on.Podman {
		t.Fatalf("podman=1: %+v, %v", on.Podman, err)
	}
	// 服务端没有带 podman：以服务端为准，视为关闭
	off, err := parseConfig("report_interval=60", on)
	if err != nil || off.Podman {
		t.Fatalf("absent podman must turn it off: %+v, %v", off.Podman, err)
	}
	off, err = parseConfig("report_interval=60&podman=0", on)
	if err != nil || off.Podman {
		t.Fatalf("podman=0: %+v, %v", off.Podman, err)
	}
	// 不合法的值整体拒绝，保持当前配置
	got, err := parseConfig("report_interval=60&podman=2", on)
	if err == nil || !got.Podman {
		t.Fatalf("invalid podman must be rejected and keep the current config: %+v, %v", got.Podman, err)
	}
	// 原来的配置对象不能被修改
	if _, err := parseConfig("report_interval=60&podman=0", on); err != nil || !on.Podman {
		t.Fatal("parseConfig must not modify the original config")
	}
}

func mkdirs(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(root, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCountPodmanAt(t *testing.T) {
	root := t.TempDir()
	// 不是 cgroup v2（没有 cgroup.controllers）：数不出来
	if _, ok := countPodmanAt(root); ok {
		t.Fatal("must report not ok without cgroup.controllers")
	}
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 还没有 machine.slice：数量为 0
	if n, ok := countPodmanAt(root); !ok || n != 0 {
		t.Fatalf("no machine.slice: %d %v", n, ok)
	}
	id := func(c string) string { return "libpod-" + c + ".scope" }
	h := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	mkdirs(t, filepath.Join(root, "machine.slice"),
		id(h), id("a"+h[1:]), id("b"+h[1:]),
		"libpod-conmon-"+h+".scope",       // 监控进程，不算
		"machine-qemu\\x2d1\\x2dvm.scope", // 虚拟机，不算
		"libpod-"+h+".scope.d",            // 不是 .scope 结尾
	)
	if n, ok := countPodmanAt(root); !ok || n != 3 {
		t.Fatalf("count = %d, ok = %v, want 3", n, ok)
	}
	// 嵌套的不数：容器里再跑 podman 产生的目录在容器自己的 cgroup 里
	mkdirs(t, filepath.Join(root, "machine.slice", id(h), "libpod_parent", id("c"+h[1:])))
	if n, _ := countPodmanAt(root); n != 3 {
		t.Fatalf("nested scopes must not be counted, got %d", n)
	}
}

func TestBuildMetricsPodman(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mkdirs(t, filepath.Join(root, "machine.slice"), "libpod-aaaa.scope", "libpod-bbbb.scope")
	old := cgroupRoot
	cgroupRoot = root
	defer func() { cgroupRoot = old }()

	a := snapshot{at: clockT0}
	b := snapshot{at: clockT0.Add(time.Second)}
	cfg := defaultConfig()
	if m := buildMetrics(a, b, cfg, &state{}, map[string]any{}, map[string]any{}); m["podman_count"] != nil {
		t.Fatalf("podman_count must not be reported when the switch is off: %v", m["podman_count"])
	}
	cfg.Podman = true
	if m := buildMetrics(a, b, cfg, &state{}, map[string]any{}, map[string]any{}); m["podman_count"] != 2 {
		t.Fatalf("podman_count = %v, want 2", m["podman_count"])
	}
	// 数不出来时（不是 cgroup v2）不上报，而不是上报 0
	cgroupRoot = t.TempDir()
	if m := buildMetrics(a, b, cfg, &state{}, map[string]any{}, map[string]any{}); m["podman_count"] != nil {
		t.Fatalf("podman_count must be absent when it cannot be counted: %v", m["podman_count"])
	}
}
