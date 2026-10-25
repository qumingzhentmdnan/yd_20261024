package prepare

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"safeagent/internal/manifest"
	"testing"
)

// TestInstallIsolation 验证独占发布、配置字节权限、重复准备和坏程序摘要。
func TestInstallIsolation(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "probe")
	data := []byte("trusted-probe")
	if err := os.WriteFile(executable, data, 0755); err != nil {
		t.Fatal(err)
	}
	plan := manifest.Plan{Home: filepath.Join(root, "home"), Executable: executable, ExecutableDigest: fmt.Sprintf("sha256:%x", sha256.Sum256(data)), Files: []manifest.File{{Path: ".config/probe/settings.json", Content: []byte("{}\n")}, {Path: ".config/probe/settings.toml", Content: []byte("enabled = true\n")}}}
	if err := Install(context.Background(), plan, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	for _, file := range plan.Files {
		target := filepath.Join(plan.Home, file.Path)
		actual, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if string(actual) != string(file.Content) {
			t.Fatal("配置字节不一致")
		}
		info, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatal(info.Mode())
		}
	}
	if err := Install(context.Background(), plan, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("重复 HOME 被覆盖")
	}
	plan.Home = filepath.Join(root, "bad")
	plan.ExecutableDigest = "sha256:wrong"
	if err := Install(context.Background(), plan, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("错误程序摘要被接受")
	}
	if _, err := os.Stat(plan.Home); !os.IsNotExist(err) {
		t.Fatal("失败留下 HOME")
	}
}

// TestInstallCancellationAndEscape 验证取消清理和受限根拒绝写出 HOME。
func TestInstallCancellationAndEscape(t *testing.T) {
	for _, scenario := range []string{"cancel", "escape"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			executable := filepath.Join(root, "probe")
			data := []byte("probe")
			if err := os.WriteFile(executable, data, 0755); err != nil {
				t.Fatal(err)
			}
			plan := manifest.Plan{Home: filepath.Join(root, "home"), Executable: executable, ExecutableDigest: fmt.Sprintf("sha256:%x", sha256.Sum256(data)), Files: []manifest.File{{Path: "../escape", Content: data}}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "cancel" {
				cancel()
			}
			if err := Install(ctx, plan, os.Getuid(), os.Getgid()); err == nil {
				t.Fatal("非法准备成功")
			}
			if _, err := os.Stat(plan.Home); !os.IsNotExist(err) {
				t.Fatal("失败未清理 HOME")
			}
			if _, err := os.Stat(filepath.Join(root, "escape")); !os.IsNotExist(err) {
				t.Fatal("写出受限根")
			}
		})
	}
}

// TestInstallPublishBoundary 验证发布前取消清理而发布后取消保持成功。
func TestInstallPublishBoundary(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprint(after), func(t *testing.T) {
			root := t.TempDir()
			data := []byte("probe")
			executable := filepath.Join(root, "probe")
			if err := os.WriteFile(executable, data, 0755); err != nil {
				t.Fatal(err)
			}
			plan := manifest.Plan{Home: filepath.Join(root, "home"), Executable: executable, ExecutableDigest: fmt.Sprintf("sha256:%x", sha256.Sum256(data)), Files: []manifest.File{{Path: ".config/settings.json", Content: []byte("{}")}}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var before, following func()
			if after {
				following = cancel
			} else {
				before = cancel
			}
			err := install(ctx, plan, os.Getuid(), os.Getgid(), before, following)
			if after {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Join(plan.Home, ".config/settings.json")); err != nil {
					t.Fatal(err)
				}
			} else {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				if _, err := os.Stat(plan.Home); !os.IsNotExist(err) {
					t.Fatal("取消留下准备")
				}
			}
		})
	}
}

// TestInstallSecondFileFailure 验证第二配置失败不留下第一配置或破坏旧准备。
func TestInstallSecondFileFailure(t *testing.T) {
	root := t.TempDir()
	data := []byte("probe")
	executable := filepath.Join(root, "probe")
	if err := os.WriteFile(executable, data, 0755); err != nil {
		t.Fatal(err)
	}
	plan := manifest.Plan{Home: filepath.Join(root, "old"), Executable: executable, ExecutableDigest: fmt.Sprintf("sha256:%x", sha256.Sum256(data)), Files: []manifest.File{{Path: "settings.json", Content: []byte("old")}}}
	if err := Install(context.Background(), plan, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	plan.Home = filepath.Join(root, "new")
	plan.Files = append(plan.Files, manifest.File{Path: "settings.json/child", Content: data})
	if err := Install(context.Background(), plan, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("错误配置成功")
	}
	if _, err := os.Stat(plan.Home); !os.IsNotExist(err) {
		t.Fatal("部分配置未清理")
	}
	actual, err := os.ReadFile(filepath.Join(root, "old/settings.json"))
	if err != nil || string(actual) != "old" {
		t.Fatal("旧准备受到影响", err)
	}
}
