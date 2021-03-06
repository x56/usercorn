package posix

import (
	"syscall"
	"time"

	co "github.com/lunixbochs/usercorn/go/kernel/common"
	"github.com/lunixbochs/usercorn/go/native"
)

func (k *PosixKernel) ClockGettime(_ int, out co.Obuf) uint64 {
	var err error
	ts := syscall.NsecToTimespec(time.Now().UnixNano())
	if k.U.Bits() == 64 {
		err = out.Pack(&native.Timespec64{int64(ts.Sec), int64(ts.Nsec)})
	} else {
		err = out.Pack(&native.Timespec{int32(ts.Sec), int32(ts.Nsec)})
	}
	if err != nil {
		return UINT64_MAX // FIXME
	}
	return 0
}
