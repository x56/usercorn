package models

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/lunixbochs/fvbommel-util/sortorder"
	uc "github.com/unicorn-engine/unicorn/bindings/go/unicorn"
)

type Reg struct {
	Enum int
	Name string
}

type RegVal struct {
	Reg
	Val uint64
}

type regList []Reg

func (r regList) Len() int      { return len(r) }
func (r regList) Swap(i, j int) { r[i], r[j] = r[j], r[i] }
func (r regList) Less(i, j int) bool {
	inum := strings.IndexAny(r[i].Name, "0123456789")
	jnum := strings.IndexAny(r[j].Name, "0123456789")
	if inum != -1 && jnum != -1 {
		return sortorder.NaturalLess(r[i].Name, r[j].Name)
	} else if inum == -1 && jnum != -1 {
		return true
	} else if jnum == -1 && inum != -1 {
		return false
	} else {
		return r[i].Name < r[j].Name
	}
}

type regMap map[int]string

func (r regMap) Items() regList {
	ret := make(regList, 0, len(r))
	for e, n := range r {
		ret = append(ret, Reg{e, n})
	}
	return ret
}

type Arch struct {
	Bits    int
	Radare  string
	CS_ARCH int
	CS_MODE uint
	UC_ARCH int
	UC_MODE int
	PC      int
	SP      int
	OS      map[string]*OS
	Regs    regMap

	// sorted for RegDump
	regList regList
}

func (a *Arch) RegisterOS(os *OS) {
	if a.OS == nil {
		a.OS = make(map[string]*OS)
	}
	if _, ok := a.OS[os.Name]; ok {
		panic("Duplicate OS " + os.Name)
	}
	a.OS[os.Name] = os
}

func (a *Arch) getRegList() regList {
	if a.regList == nil {
		rl := a.Regs.Items()
		sort.Sort(rl)
		a.regList = rl
	}
	return a.regList
}

func (a *Arch) SmokeTest(t *testing.T) {
	u, err := uc.NewUnicorn(a.UC_ARCH, a.UC_MODE)
	if err != nil {
		t.Fatal(err)
	}
	var testReg = func(name string, enum int) {
		if u.RegWrite(enum, 0x1000); err != nil {
			t.Fatal(err)
		}
		val, err := u.RegRead(enum)
		if err != nil {
			t.Fatal(err)
		}
		if val != 0x1000 {
			t.Fatal(a.Radare + " failed to read/write register " + name)
		}
		// clear the register in case registers are aliased
		if u.RegWrite(enum, 0); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range a.getRegList() {
		testReg(r.Name, r.Enum)
	}
	testReg("PC", a.PC)
	testReg("SP", a.SP)
}

func (a *Arch) RegDump(u uc.Unicorn) ([]RegVal, error) {
	ret := make([]RegVal, len(a.Regs))
	for i, r := range a.getRegList() {
		val, err := u.RegRead(r.Enum)
		if err != nil {
			return nil, err
		}
		ret[i] = RegVal{r, val}
	}
	return ret, nil
}

type OS struct {
	Name      string
	Kernels   func(Usercorn) []interface{}
	Init      func(Usercorn, []string, []string) error
	Interrupt func(Usercorn, uint32)
}

func (o *OS) String() string {
	return fmt.Sprintf("<OS %s>", o.Name)
}
