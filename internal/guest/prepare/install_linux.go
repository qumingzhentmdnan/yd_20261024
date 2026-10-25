// Package prepare 将可信清单的配置安装到每次准备独占的 HOME。
package prepare

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"safeagent/internal/manifest"
)

// Install 在可信父目录下创建全新 HOME，最后一次取消检查后发布访问权限。
func Install(ctx context.Context, plan manifest.Plan, uid, gid int) (err error) {
	return install(ctx, plan, uid, gid, nil, nil)
}

// install 让包内测试在实际发布边界注入取消，生产入口不安装闸门。
func install(ctx context.Context, plan manifest.Plan, uid, gid int, beforePublish, afterPublish func()) (err error) {
	executable, err := os.Open(plan.Executable)
	if err != nil {
		return err
	}
	// 原生 CLI 可达数百 MB，流式校验避免在每次 Prepare 中复制整份程序。
	checksum := sha256.New()
	_, hashErr := io.Copy(checksum, executable)
	closeErr := executable.Close()
	if hashErr != nil {
		return hashErr
	}
	if closeErr != nil {
		return closeErr
	}
	if fmt.Sprintf("sha256:%x", checksum.Sum(nil)) != plan.ExecutableDigest {
		return fmt.Errorf("executable digest mismatch")
	}
	parent, err := os.OpenRoot(filepath.Dir(plan.Home))
	if err != nil {
		return err
	}
	defer parent.Close()
	name := filepath.Base(plan.Home)
	if err = parent.Mkdir(name, 0700); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			err = errors.Join(err, parent.RemoveAll(name))
		}
	}()
	home, err := parent.OpenRoot(name)
	if err != nil {
		return err
	}
	defer home.Close()
	directories := map[string]bool{".": true}
	for _, file := range plan.Files {
		if err = ctx.Err(); err != nil {
			return err
		}
		dir := filepath.Dir(file.Path)
		if err = home.MkdirAll(dir, 0700); err != nil {
			return err
		}
		for d := dir; d != "."; d = filepath.Dir(d) {
			directories[d] = true
		}
		target, openErr := home.OpenFile(file.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if openErr != nil {
			return openErr
		}
		_, err = target.Write(file.Content)
		if err == nil {
			err = target.Chown(uid, gid)
		}
		closeErr := target.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	// HOME 根仍归 root 且为0700，子目录换主期间 Agent 无法访问。
	for dir := range directories {
		if dir == "." {
			continue
		}
		if err = home.Chown(dir, uid, gid); err != nil {
			return err
		}
	}
	if beforePublish != nil {
		beforePublish()
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	// 根目录换主是提交边界，之后取消不得回滚已经发布的准备。
	if err = home.Chown(".", uid, gid); err != nil {
		return err
	}
	committed = true
	if afterPublish != nil {
		afterPublish()
	}
	return nil
}
