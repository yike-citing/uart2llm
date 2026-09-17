//go:build !windows

package credentials

import "errors"

func New(string) (Store, error) {
	return nil, errors.New("production credential storage currently supports Windows only")
}
