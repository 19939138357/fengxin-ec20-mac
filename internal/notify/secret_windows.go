//go:build windows

package notify

import (
	"golang.org/x/sys/windows"
	"unsafe"
)

func protect(secret []byte) ([]byte, error)              { return crypt(secret, true) }
func unprotect(secret []byte) ([]byte, error)            { return crypt(secret, false) }
func unprotectInteractive(secret []byte) ([]byte, error) { return unprotect(secret) }

func crypt(value []byte, seal bool) ([]byte, error) {
	if len(value) == 0 {
		return nil, nil
	}
	in := windows.DataBlob{Size: uint32(len(value)), Data: &value[0]}
	var out windows.DataBlob
	var err error
	if seal {
		err = windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	} else {
		err = windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	}
	if err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, int(out.Size))...), nil
}
