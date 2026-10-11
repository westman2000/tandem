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
	switch info.Type {
	case unix.NFS_SUPER_MAGIC, unix.CIFS_SUPER_MAGIC, unix.SMB2_SUPER_MAGIC:
		return errors.New("Для рабочей базы выберите локальный диск. NFS/SMB не поддерживаются")
	}
	return nil
}
