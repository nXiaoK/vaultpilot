package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func restoreTargetError(err error, path string) error {
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS) {
		return fail("RESTORE_TARGET_NOT_WRITABLE", fmt.Sprintf("恢复目标不可写：%s。当前进程 UID=%d、GID=%d；请让该用户对目标及父目录具有写入和进入权限。Docker 部署请检查 VP_RESTORE_HOST 对应宿主机目录的属主或 ACL，以及挂载是否只读。原始错误：%v", path, os.Geteuid(), os.Getegid(), err))
	}
	return err
}

// Check the nearest existing directory using effective credentials, including
// supplementary groups and filesystem ACLs. Preview must not create probe files
// or directories. Recheck on every preview; execution still handles later changes.
func checkRestoreDirectoryWritable(root *os.Root, directory string, checked map[string]bool) error {
	requested := directory
	for {
		if checked[directory] {
			if len(checked) < 4096 {
				checked[requested] = true
			}
			return nil
		}
		path := filepath.Join(root.Name(), directory)
		file, err := root.Open(directory)
		if errors.Is(err, os.ErrNotExist) && directory != "." {
			directory = filepath.Dir(directory)
			continue
		}
		if err != nil {
			return restoreTargetError(err, path)
		}
		info, err := file.Stat()
		if err == nil && !info.IsDir() {
			err = fail("RESTORE_UNSAFE_PARENT", "恢复路径的父目录不是普通目录："+path)
		}
		if err == nil {
			err = unix.Faccessat(int(file.Fd()), ".", unix.W_OK|unix.X_OK, unix.AT_EACCESS)
		}
		file.Close()
		if err != nil {
			return restoreTargetError(err, path)
		}
		// Keep preflight memory bounded even for millions of distinct directories.
		if len(checked) < 4096 {
			checked[directory] = true
		}
		if len(checked) < 4096 {
			checked[requested] = true
		}
		return nil
	}
}
