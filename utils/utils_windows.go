// +build windows

package utils

import "github.com/pkg/errors"

func RunUnderSystemdScope(pid int, slice string, unitName string) error {
	return errors.New("not implemented for windows")
}

func MoveUnderCgroup2Subtree(subtree string) error {
	return errors.New("not implemented for windows")
}

func GetPidCgroupv2(pid int) (string, error) {
	return "", errors.New("not implemented for windows")
}
