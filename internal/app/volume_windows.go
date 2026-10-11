package app

import (
	"errors"
	"golang.org/x/sys/windows"
	"path/filepath"
)

func localProjectVolume(path string) error {
	root, err := windows.UTF16PtrFromString(filepath.VolumeName(path) + `\`)
	if err != nil {
		return err
	}
	if windows.GetDriveType(root) == windows.DRIVE_REMOTE {
		return errors.New("Для рабочей базы выберите локальный диск. Сетевая папка не поддерживается")
	}
	return nil
}
