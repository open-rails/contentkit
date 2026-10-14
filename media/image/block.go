package image

/*
#cgo pkg-config: vips
#include <stdlib.h>
#include <vips/vips.h>

#define CK_CAN_BLOCK (VIPS_MAJOR_VERSION > 8 || (VIPS_MAJOR_VERSION == 8 && VIPS_MINOR_VERSION >= 13))

static int ck_block(const char *name, int state) {
#if CK_CAN_BLOCK
	vips_operation_block_set(name, state);
	return 0;
#else
	return -1;
#endif
}

static int ck_block_untrusted(void) {
#if CK_CAN_BLOCK
	vips_block_untrusted_set(TRUE);
	return 0;
#else
	return -1;
#endif
}
*/
import "C"

import (
	"errors"
	"unsafe"
)

// loaders are the libvips loaders of formats; every other is blocked.
var loaders = []string{"VipsForeignLoadJpeg", "VipsForeignLoadPng", "VipsForeignLoadWebp",
	"VipsForeignLoadNsgif", "VipsForeignLoadHeif", "VipsForeignLoadTiff"}

// blockLoaders leaves libvips only the loaders it trusts among loaders, so
// no upload reaches ImageMagick, librsvg, poppler and the like. A libvips
// that cannot block (before 8.13) fails startup instead of running open.
func blockLoaders() error {
	block := func(name string, state C.int) C.int {
		s := C.CString(name)
		defer C.free(unsafe.Pointer(s))
		return C.ck_block(s, state)
	}
	if block("VipsForeignLoad", 1) != 0 {
		return errors.New("libvips 8.13 or later is required to block untrusted loaders")
	}
	for _, l := range loaders {
		block(l, 0)
	}
	C.ck_block_untrusted() // after the allowlist, so it stays blocked if libvips distrusts one
	return nil
}
