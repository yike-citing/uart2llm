//go:build !windows

package platform

import "errors"

func Singleton(string) (func(), error) {
	return nil, errors.New("daemon release supports Windows only")
}
