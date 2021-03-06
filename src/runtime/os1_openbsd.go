// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

const (
	_ESRCH       = 3
	_EAGAIN      = 35
	_EWOULDBLOCK = _EAGAIN
	_ENOTSUP     = 91

	// From OpenBSD's sys/time.h
	_CLOCK_REALTIME  = 0
	_CLOCK_VIRTUAL   = 1
	_CLOCK_PROF      = 2
	_CLOCK_MONOTONIC = 3
)

const (
	sigset_none = uint32(0)
	sigset_all  = ^uint32(0)
)

// From OpenBSD's <sys/sysctl.h>
const (
	_CTL_HW  = 6
	_HW_NCPU = 3
)

func getncpu() int32 {
	mib := [2]uint32{_CTL_HW, _HW_NCPU}
	out := uint32(0)
	nout := unsafe.Sizeof(out)

	// Fetch hw.ncpu via sysctl.
	ret := sysctl(&mib[0], 2, (*byte)(unsafe.Pointer(&out)), &nout, nil, 0)
	if ret >= 0 {
		return int32(out)
	}
	return 1
}

//go:nosplit
func semacreate(mp *m) {
}

//go:nosplit
func semasleep(ns int64) int32 {
	_g_ := getg()

	// Compute sleep deadline.
	var tsp *timespec
	if ns >= 0 {
		var ts timespec
		var nsec int32
		ns += nanotime()
		ts.set_sec(int64(timediv(ns, 1000000000, &nsec)))
		ts.set_nsec(nsec)
		tsp = &ts
	}

	for {
		v := atomic.Load(&_g_.m.waitsemacount)
		if v > 0 {
			if atomic.Cas(&_g_.m.waitsemacount, v, v-1) {
				return 0 // semaphore acquired
			}
			continue
		}

		// Sleep until woken by semawakeup or timeout; or abort if waitsemacount != 0.
		//
		// From OpenBSD's __thrsleep(2) manual:
		// "The abort argument, if not NULL, points to an int that will
		// be examined [...] immediately before blocking.  If that int
		// is non-zero then __thrsleep() will immediately return EINTR
		// without blocking."
		ret := thrsleep(uintptr(unsafe.Pointer(&_g_.m.waitsemacount)), _CLOCK_MONOTONIC, tsp, 0, &_g_.m.waitsemacount)
		if ret == _EWOULDBLOCK {
			return -1
		}
	}
}

//go:nosplit
func semawakeup(mp *m) {
	atomic.Xadd(&mp.waitsemacount, 1)
	ret := thrwakeup(uintptr(unsafe.Pointer(&mp.waitsemacount)), 1)
	if ret != 0 && ret != _ESRCH {
		// semawakeup can be called on signal stack.
		systemstack(func() {
			print("thrwakeup addr=", &mp.waitsemacount, " sem=", mp.waitsemacount, " ret=", ret, "\n")
		})
	}
}

// May run with m.p==nil, so write barriers are not allowed.
//go:nowritebarrier
func newosproc(mp *m, stk unsafe.Pointer) {
	if false {
		print("newosproc stk=", stk, " m=", mp, " g=", mp.g0, " id=", mp.id, " ostk=", &mp, "\n")
	}

	param := tforkt{
		tf_tcb:   unsafe.Pointer(&mp.tls[0]),
		tf_tid:   (*int32)(unsafe.Pointer(&mp.procid)),
		tf_stack: uintptr(stk),
	}

	oset := sigprocmask(_SIG_SETMASK, sigset_all)
	ret := tfork(&param, unsafe.Sizeof(param), mp, mp.g0, funcPC(mstart))
	sigprocmask(_SIG_SETMASK, oset)

	if ret < 0 {
		print("runtime: failed to create new OS thread (have ", mcount()-1, " already; errno=", -ret, ")\n")
		throw("runtime.newosproc")
	}
}

func osinit() {
	ncpu = getncpu()
}

var urandom_dev = []byte("/dev/urandom\x00")

//go:nosplit
func getRandomData(r []byte) {
	fd := open(&urandom_dev[0], 0 /* O_RDONLY */, 0)
	n := read(fd, unsafe.Pointer(&r[0]), int32(len(r)))
	closefd(fd)
	extendRandom(r, int(n))
}

func goenvs() {
	goenvs_unix()
}

// Called to initialize a new m (including the bootstrap m).
// Called on the parent thread (main thread in case of bootstrap), can allocate memory.
func mpreinit(mp *m) {
	mp.gsignal = malg(32 * 1024)
	mp.gsignal.m = mp
}

func msigsave(mp *m) {
	smask := (*uint32)(unsafe.Pointer(&mp.sigmask))
	if unsafe.Sizeof(*smask) > unsafe.Sizeof(mp.sigmask) {
		throw("insufficient storage for signal mask")
	}
	*smask = sigprocmask(_SIG_BLOCK, 0)
}

// Called to initialize a new m (including the bootstrap m).
// Called on the new thread, can not allocate memory.
func minit() {
	_g_ := getg()

	// m.procid is a uint64, but tfork writes an int32. Fix it up.
	_g_.m.procid = uint64(*(*int32)(unsafe.Pointer(&_g_.m.procid)))

	// Initialize signal handling
	signalstack(&_g_.m.gsignal.stack)

	// restore signal mask from m.sigmask and unblock essential signals
	nmask := *(*uint32)(unsafe.Pointer(&_g_.m.sigmask))
	for i := range sigtable {
		if sigtable[i].flags&_SigUnblock != 0 {
			nmask &^= 1 << (uint32(i) - 1)
		}
	}
	sigprocmask(_SIG_SETMASK, nmask)
}

// Called from dropm to undo the effect of an minit.
func unminit() {
	_g_ := getg()
	smask := *(*uint32)(unsafe.Pointer(&_g_.m.sigmask))
	sigprocmask(_SIG_SETMASK, smask)
	signalstack(nil)
}

func memlimit() uintptr {
	return 0
}

func sigtramp()

type sigactiont struct {
	sa_sigaction uintptr
	sa_mask      uint32
	sa_flags     int32
}

func setsig(i int32, fn uintptr, restart bool) {
	var sa sigactiont
	sa.sa_flags = _SA_SIGINFO | _SA_ONSTACK
	if restart {
		sa.sa_flags |= _SA_RESTART
	}
	sa.sa_mask = sigset_all
	if fn == funcPC(sighandler) {
		fn = funcPC(sigtramp)
	}
	sa.sa_sigaction = fn
	sigaction(i, &sa, nil)
}

func setsigstack(i int32) {
	throw("setsigstack")
}

func getsig(i int32) uintptr {
	var sa sigactiont
	sigaction(i, nil, &sa)
	if sa.sa_sigaction == funcPC(sigtramp) {
		return funcPC(sighandler)
	}
	return sa.sa_sigaction
}

func signalstack(s *stack) {
	var st stackt
	if s == nil {
		st.ss_flags = _SS_DISABLE
	} else {
		st.ss_sp = s.lo
		st.ss_size = s.hi - s.lo
		st.ss_flags = 0
	}
	sigaltstack(&st, nil)
}

func updatesigmask(m sigmask) {
	sigprocmask(_SIG_SETMASK, m[0])
}

func unblocksig(sig int32) {
	mask := uint32(1) << (uint32(sig) - 1)
	sigprocmask(_SIG_UNBLOCK, mask)
}
