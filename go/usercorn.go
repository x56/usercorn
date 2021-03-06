package usercorn

import (
	"errors"
	"fmt"
	uc "github.com/unicorn-engine/unicorn/bindings/go/unicorn"
	"os"
	"path/filepath"
	"strings"

	"github.com/lunixbochs/usercorn/go/arch"
	"github.com/lunixbochs/usercorn/go/kernel/common"
	"github.com/lunixbochs/usercorn/go/loader"
	"github.com/lunixbochs/usercorn/go/models"
)

type Usercorn struct {
	*Unicorn
	exe          string
	loader       models.Loader
	interpLoader models.Loader
	kernels      []common.Kernel

	base       uint64
	interpBase uint64
	entry      uint64
	binEntry   uint64

	StackBase uint64
	brk       uint64

	Verbose         bool
	TraceSys        bool
	TraceMem        bool
	TraceMemBatch   bool
	TraceExec       bool
	TraceReg        bool
	TraceMatch      []string
	TraceMatchDepth int
	traceMatching   bool
	ForceBase       uint64
	ForceInterpBase uint64
	LoopCollapse    int
	Demangle        bool

	LoadPrefix string
	status     models.StatusDiff
	stacktrace models.Stacktrace
	blockloop  *models.LoopDetect
	memlog     models.MemLog

	exitStatus error

	// deadlock detection
	lastBlock uint64
	lastCode  uint64
	deadlock  int
}

func NewUsercorn(exe string, prefix string) (*Usercorn, error) {
	l, err := loader.LoadFile(exe)
	if err != nil {
		return nil, err
	}
	a, os, err := arch.GetArch(l.Arch(), l.OS())
	if err != nil {
		return nil, err
	}
	unicorn, err := NewUnicorn(a, os, l.ByteOrder())
	if err != nil {
		return nil, err
	}
	exe, _ = filepath.Abs(exe)
	u := &Usercorn{
		Unicorn:       unicorn,
		exe:           exe,
		loader:        l,
		LoadPrefix:    prefix,
		traceMatching: true,
	}
	// load kernels
	// the array cast is a trick to work around circular imports
	if os.Kernels != nil {
		kernelI := os.Kernels(u)
		kernels := make([]common.Kernel, len(kernelI))
		for i, k := range kernelI {
			kernels[i] = k.(common.Kernel)
		}
		u.kernels = kernels
	}
	// map binary (and interp) into memory
	u.status = models.StatusDiff{U: u, Color: true}
	u.interpBase, u.entry, u.base, u.binEntry, err = u.mapBinary(u.loader, false)
	if err != nil {
		return nil, err
	}
	// find data segment for brk
	segments, err := l.Segments()
	if err != nil {
		return nil, err
	}
	for _, seg := range segments {
		if seg.Prot&uc.PROT_WRITE != 0 {
			addr := u.base + seg.Addr + seg.Size
			if addr > u.brk {
				u.brk = addr
			}
		}
	}
	// TODO: have a "host page size", maybe arch.Align()
	mask := uint64(4096 - 1)
	u.Brk((u.brk + mask) & ^mask)
	return u, nil
}

func (u *Usercorn) Run(args []string, env []string) error {
	if u.LoopCollapse > 0 {
		u.blockloop = models.NewLoopDetect(u.LoopCollapse)
	}
	if err := u.addHooks(); err != nil {
		return err
	}
	if err := u.mapStack(); err != nil {
		return err
	}
	if u.os.Init != nil {
		if err := u.os.Init(u, args, env); err != nil {
			return err
		}
	}
	if u.Verbose {
		fmt.Fprintf(os.Stderr, "[entry @ 0x%x]\n", u.entry)
		dis, err := u.Disas(u.entry, 64)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		} else {
			fmt.Fprintln(os.Stderr, dis)
		}
		sp, err := u.RegRead(u.arch.SP)
		if err != nil {
			return err
		}
		buf := make([]byte, u.StackBase+STACK_SIZE-sp)
		if err := u.MemReadInto(buf, sp); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "[stack @ 0x%x]\n", sp)
		for _, line := range models.HexDump(sp, buf[:], u.arch.Bits) {
			fmt.Fprintf(os.Stderr, "%s\n", line)
		}
	}
	if u.Verbose || u.TraceReg {
		u.status.Changes().Print("", true, false)
	}
	if u.Verbose {
		fmt.Fprintln(os.Stderr, "=====================================")
		fmt.Fprintln(os.Stderr, "==== Program output begins here. ====")
		fmt.Fprintln(os.Stderr, "=====================================")
	}
	if u.TraceReg || u.TraceExec {
		sp, _ := u.RegRead(u.arch.SP)
		sym, _ := u.Symbolicate(u.entry)
		u.stacktrace.Update(u.entry, sp, sym)
	}
	if u.TraceMemBatch {
		u.memlog = *models.NewMemLog(u.ByteOrder())
	}
	err := u.Unicorn.Start(u.entry, 0xffffffffffffffff)
	if u.TraceMemBatch && !u.memlog.Empty() {
		u.memlog.Print("", u.arch.Bits)
		u.memlog.Reset()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Registers:")
		u.status.Changes().Print("", true, false)
		fmt.Fprintln(os.Stderr, "Stacktrace:")
		u.stacktrace.Print(u)
	}
	if err == nil && u.exitStatus != nil {
		err = u.exitStatus
	}
	return err
}

func (u *Usercorn) Exe() string {
	return u.exe
}

func (u *Usercorn) Loader() models.Loader {
	return u.loader
}

func (u *Usercorn) InterpBase() uint64 {
	// points to interpreter base or 0
	return u.interpBase
}

func (u *Usercorn) Base() uint64 {
	// points to program base
	return u.base
}

func (u *Usercorn) Entry() uint64 {
	// points to effective program entry: either an interpreter or the binary
	return u.entry
}

func (u *Usercorn) BinEntry() uint64 {
	// points to binary entry, even if an interpreter is used
	return u.binEntry
}

func (u *Usercorn) PrefixPath(path string, force bool) string {
	if filepath.IsAbs(path) && u.LoadPrefix != "" {
		target := filepath.Join(u.LoadPrefix, path)
		_, err := os.Stat(target)
		exists := !os.IsNotExist(err)
		if force || exists {
			return target
		}
	}
	return path
}

func (u *Usercorn) Symbolicate(addr uint64) (string, error) {
	var symbolicate = func(addr uint64, symbols []models.Symbol) (result models.Symbol, distance uint64) {
		if len(symbols) == 0 {
			return
		}
		nearest := make(map[uint64][]models.Symbol)
		var min int64 = -1
		for _, sym := range symbols {
			if sym.Start == 0 {
				continue
			}
			dist := int64(addr - sym.Start)
			if dist >= 0 && (sym.Start+uint64(dist) <= sym.End || sym.End == 0) && sym.Name != "" {
				if dist < min || min == -1 {
					min = dist
				}
				nearest[uint64(dist)] = append(nearest[uint64(dist)], sym)
			}
		}
		if len(nearest) > 0 {
			sym := nearest[uint64(min)][0]
			return sym, uint64(min)
		}
		return
	}
	symbols, _ := u.loader.Symbols()
	var interpSym []models.Symbol
	if u.interpLoader != nil {
		interpSym, _ = u.interpLoader.Symbols()
	}
	sym, sdist := symbolicate(addr-u.base, symbols)
	isym, idist := symbolicate(addr-u.interpBase, interpSym)
	if idist < sdist && isym.Name != "" || sym.Name == "" {
		sym = isym
		sdist = idist
	}
	if sym.Name != "" {
		if u.Demangle {
			sym.Name = models.Demangle(sym.Name)
		}
		if sdist > 0 {
			return fmt.Sprintf("%s+0x%x", sym.Name, sdist), nil
		}
	}
	return sym.Name, nil
}

func (u *Usercorn) Brk(addr uint64) (uint64, error) {
	// TODO: this is linux specific
	if addr > 0 {
		err := u.MemMapProt(u.brk, addr-u.brk, uc.PROT_READ|uc.PROT_WRITE)
		if err != nil {
			return u.brk, err
		}
		u.brk = addr
	}
	return u.brk, nil
}

func (u *Usercorn) checkTraceMatch(addr uint64, sym string) bool {
	if len(u.TraceMatch) == 0 {
		return true
	}
	match := func(addr uint64, sym string, trace string) bool {
		return sym == trace || strings.HasPrefix(sym, trace+"+") || fmt.Sprintf("0x%x", addr) == strings.ToLower(trace)
	}
	for _, v := range u.TraceMatch {
		if match(addr, sym, v) {
			return true
		}
	}
	l := u.stacktrace.Len()
	for i := 0; i < u.TraceMatchDepth && i < l; i++ {
		frame := u.stacktrace.Stack[l-i-1]
		for _, v := range u.TraceMatch {
			if match(frame.PC, frame.Sym, v) {
				return true
			}
		}
	}
	return false
}

func (u *Usercorn) addHooks() error {
	if u.TraceExec || u.TraceReg {
		u.HookAdd(uc.HOOK_BLOCK, func(_ uc.Unicorn, addr uint64, size uint32) {
			sym, _ := u.Symbolicate(addr)
			if !u.checkTraceMatch(addr, sym) {
				u.traceMatching = false
				return
			}
			u.traceMatching = true
			var indent string
			if u.stacktrace.Len() > 2 {
				indent = strings.Repeat("  ", u.stacktrace.Len()-1)
			}
			if u.blockloop != nil {
				if looped, loop, count := u.blockloop.Update(addr); looped {
					return
				} else if count > 1 {
					// TODO: maybe print a message when we start collapsing loops
					// with the symbols or even all disassembly involved encapsulated
					chain := u.blockloop.String(u, loop)
					fmt.Fprintf(os.Stderr, indent+"- (%d) loops over %s\n", count, chain)
				}
			}
			if u.TraceMemBatch {
				u.memlog.Print(indent, u.arch.Bits)
				u.memlog.Reset()
			}
			if sp, err := u.RegRead(u.arch.SP); err == nil {
				u.stacktrace.Update(addr, sp, sym)
			}
			indent = strings.Repeat("  ", u.stacktrace.Len())
			blockIndent := indent
			if len(indent) >= 2 {
				blockIndent = indent[:len(indent)-2]
			}
			if sym != "" {
				sym = " (" + sym + ")"
			}
			blockLine := fmt.Sprintf("\n%s+ block%s @0x%x", blockIndent, sym, addr)
			if !u.TraceExec && u.TraceReg && u.deadlock == 0 {
				changes := u.status.Changes()
				if changes.Count() > 0 {
					fmt.Fprintln(os.Stderr, blockLine)
					changes.Print(indent, true, true)
				}
			} else {
				fmt.Fprintln(os.Stderr, blockLine)
			}
			u.lastBlock = addr
		})
	}
	if u.TraceExec {
		u.HookAdd(uc.HOOK_CODE, func(_ uc.Unicorn, addr uint64, size uint32) {
			if !u.traceMatching {
				return
			}
			indent := strings.Repeat("  ", u.stacktrace.Len())
			var changes *models.Changes
			if addr == u.lastCode || u.TraceReg && u.TraceExec {
				changes = u.status.Changes()
			}
			if u.TraceExec && u.blockloop == nil || u.blockloop.Loops == 0 {
				dis, _ := u.Disas(addr, uint64(size))
				fmt.Fprintf(os.Stderr, "%s", indent+dis)
				if !u.TraceReg || changes.Count() == 0 {
					fmt.Fprintln(os.Stderr)
				} else {
					dindent := ""
					// TODO: I can count the max dis length in the block and reuse it here
					pad := 40 - len(dis)
					if pad > 0 {
						dindent = strings.Repeat(" ", pad)
					}
					changes.Print(dindent, true, true)
				}
			}
			if addr == u.lastCode {
				u.deadlock++
				if changes.Count() > 0 {
					if u.TraceReg {
						changes.Print(indent, true, true)
					}
					u.deadlock = 0
				}
				if u.deadlock > 2 {
					sym, _ := u.Symbolicate(addr)
					if sym != "" {
						sym = " (" + sym + ")"
					}
					fmt.Fprintf(os.Stderr, "FATAL: deadlock detected at 0x%x%s\n", addr, sym)
					changes.Print(indent, true, false)
					u.Stop()
				}
			} else {
				u.deadlock = 0
			}
			u.lastCode = addr
		})
	}
	if u.TraceMem || u.TraceMemBatch {
		u.HookAdd(uc.HOOK_MEM_READ|uc.HOOK_MEM_WRITE, func(_ uc.Unicorn, access int, addr uint64, size int, value int64) {
			if !u.traceMatching {
				return
			}
			var letter string
			if access == uc.MEM_WRITE {
				letter = "W"
			} else {
				letter = "R"
				if data, err := u.MemRead(addr, uint64(size)); err == nil {
					e := u.ByteOrder()
					switch size {
					case 1:
						value = int64(data[0])
					case 2:
						value = int64(e.Uint16(data))
					case 4:
						value = int64(e.Uint32(data))
					case 8:
						value = int64(e.Uint64(data))
					}
				}
			}
			if u.TraceMem {
				memFmt := fmt.Sprintf("%%s%%s 0x%%0%dx 0x%%0%dx\n", u.Bsz*2, size*2)
				indent := ""
				if u.stacktrace.Len() > 0 {
					indent = strings.Repeat("  ", u.stacktrace.Len()-1)
				}
				fmt.Fprintf(os.Stderr, memFmt, indent, letter, addr, value)
			}
			if u.TraceMemBatch {
				write := (letter == "W")
				if !(u.TraceExec || u.TraceReg) && !u.memlog.Adjacent(addr, value, size, write) {
					u.memlog.Print("", u.arch.Bits)
					u.memlog.Reset()
				}
				u.memlog.Update(addr, size, value, letter == "W")
			}
		})
	}
	invalid := uc.HOOK_MEM_READ_INVALID | uc.HOOK_MEM_WRITE_INVALID | uc.HOOK_MEM_FETCH_INVALID
	u.HookAdd(invalid, func(_ uc.Unicorn, access int, addr uint64, size int, value int64) bool {
		switch access {
		case uc.MEM_WRITE_UNMAPPED, uc.MEM_WRITE_PROT:
			fmt.Fprintf(os.Stderr, "invalid write")
		case uc.MEM_READ_UNMAPPED, uc.MEM_READ_PROT:
			fmt.Fprintf(os.Stderr, "invalid read")
		case uc.MEM_FETCH_UNMAPPED, uc.MEM_FETCH_PROT:
			fmt.Fprintf(os.Stderr, "invalid fetch")
		default:
			fmt.Fprintf(os.Stderr, "unknown memory error")
		}
		fmt.Fprintf(os.Stderr, ": @0x%x, 0x%x = 0x%x\n", addr, size, uint64(value))
		return false
	})
	u.HookAdd(uc.HOOK_INTR, func(_ uc.Unicorn, intno uint32) {
		u.os.Interrupt(u, intno)
	})
	return nil
}

func (u *Usercorn) mapBinary(l models.Loader, isInterp bool) (interpBase, entry, base, realEntry uint64, err error) {
	var dynamic bool
	switch l.Type() {
	case loader.EXEC:
		dynamic = false
	case loader.DYN:
		dynamic = true
	default:
		err = errors.New("Unsupported file load type.")
		return
	}
	segments, err := l.Segments()
	if err != nil {
		return
	}
	// merge overlapping segments
	merged := make([]*models.Segment, 0, len(segments))
outer:
	for _, seg := range segments {
		addr, size := align(seg.Addr, seg.Size, true)
		s := &models.Segment{addr, addr + size, seg.Prot}
		for _, s2 := range merged {
			if s2.Overlaps(s) {
				s2.Merge(s)
				continue outer
			}
		}
		merged = append(merged, s)
	}
	// map merged segments
	loadBias := u.ForceBase
	if isInterp {
		loadBias = u.ForceInterpBase
	}
	for _, seg := range merged {
		size := seg.End - seg.Start
		if dynamic && seg.Start == 0 && loadBias == 0 {
			loadBias, err = u.Mmap(0x1000000, size)
		} else {
			prot := seg.Prot
			if prot == 0 {
				prot = uc.PROT_ALL
			}
			err = u.MemMapProt(loadBias+seg.Start, seg.End-seg.Start, prot)
		}
		if err != nil {
			return
		}
	}
	// write segment memory
	var data []byte
	for _, seg := range segments {
		if data, err = seg.Data(); err != nil {
			return
		}
		if err = u.MemWrite(loadBias+seg.Addr, data); err != nil {
			return
		}
	}
	entry = loadBias + l.Entry()
	// load interpreter if present
	interp := l.Interp()
	if interp != "" && !isInterp {
		var bin models.Loader
		bin, err = loader.LoadFileArch(u.PrefixPath(interp, true), l.Arch())
		if err != nil {
			return
		}
		if bin.Arch() != l.Arch() {
			err = fmt.Errorf("Interpreter arch mismatch: %s != %s", l.Arch() != bin.Arch())
			return
		}
		u.interpLoader = bin
		_, _, interpBias, interpEntry, err := u.mapBinary(bin, true)
		return interpBias, interpEntry, loadBias, entry, err
	} else {
		return 0, entry, loadBias, entry, nil
	}
}

func (u *Usercorn) mapStack() error {
	stack, err := u.Mmap(STACK_BASE, STACK_SIZE)
	if err != nil {
		return err
	}
	u.StackBase = stack
	stackEnd := stack + STACK_SIZE
	if err := u.RegWrite(u.arch.SP, stackEnd); err != nil {
		return err
	}
	return u.MemMapProt(stackEnd, UC_MEM_ALIGN, uc.PROT_NONE)
}

func (u *Usercorn) Syscall(num int, name string, getArgs func(n int) ([]uint64, error)) (uint64, error) {
	if name == "" {
		panic(fmt.Sprintf("Syscall missing: %d", num))
	}
	if u.TraceSys && u.stacktrace.Len() > 0 {
		fmt.Fprintf(os.Stderr, strings.Repeat("  ", u.stacktrace.Len()-1)+"s ")
	}
	for _, k := range u.kernels {
		if sys := k.UsercornSyscall(name); sys != nil {
			args, err := getArgs(len(sys.In))
			if err != nil {
				return 0, err
			}
			if u.TraceSys {
				sys.Trace(args)
			}
			ret := sys.Call(args)
			if u.TraceSys {
				sys.TraceRet(args, ret)
			}
			return ret, nil
		}
	}
	panic(fmt.Errorf("Kernel not found for syscall '%s'", name))
}

func (u *Usercorn) Exit(status int) {
	u.exitStatus = models.ExitStatus(status)
	u.Stop()
}
