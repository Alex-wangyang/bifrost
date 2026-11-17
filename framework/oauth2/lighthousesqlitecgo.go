//go:build cgo

package oauth2

import (
	"errors"
	"github.com/mattn/go-sqlite3"
)

func isLighthouseSQLiteLock(err error) bool {
	var e sqlite3.Error
	return errors.As(err, &e) && (e.Code == sqlite3.ErrBusy || e.Code == sqlite3.ErrLocked)
}
