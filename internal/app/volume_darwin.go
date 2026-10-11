package app

import (
	"errors"
	"golang.org/x/sys/unix"
)

func localProjectVolume(path string) error {
	var info unix.Statfs_t
	if err := unix.Statfs(path, &info); err != nil {
		return err
	}
	if info.Flags&unix.MNT_LOCAL == 0 {
		return errors.New("Для рабочей базы выберите локальный диск. Сетевая папка не поддерживается")
	}
	return nil
}
