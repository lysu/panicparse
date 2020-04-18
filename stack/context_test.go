// Copyright 2018 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package stack

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/maruel/panicparse/v2/internal/internaltest"
)

func TestParseDumpNothing(t *testing.T) {
	t.Parallel()
	extra := &bytes.Buffer{}
	c := NewContext()
	if err := c.ParseDump(context.Background(), bytes.NewBufferString("\n"), extra); err != nil {
		t.Fatal(err)
	}
	c.GuessPaths()
	if c.Goroutines != nil {
		t.Fatalf("unexpected %v", c)
	}
}

type blockingRead struct {
	i int
	c chan struct{}
}

func (b *blockingRead) Read(buf []byte) (int, error) {
	b.i++
	if b.i == 1 {
		copy(buf, "allo")
		return 4, nil
	}
	<-b.c
	return 0, nil
}

func TestParseDumpCanceled(t *testing.T) {
	t.Parallel()
	extra := &bytes.Buffer{}
	ctx, cancel := context.WithCancel(context.Background())
	in := blockingRead{c: make(chan struct{})}

	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer func() {
			done <- struct{}{}
		}()
		started <- struct{}{}
		c := NewContext()
		if err := c.ParseDump(ctx, &in, extra); err != context.Canceled {
			t.Error(err)
		}
		if c.Goroutines != nil {
			t.Errorf("unexpected %v", c)
		}
	}()

	<-started
	cancel()
	<-done
}

func TestParseDump1(t *testing.T) {
	t.Parallel()
	// One call from main, one from stdlib, one from third party.
	// Create a long first line that will be ignored. It is to guard against
	// https://github.com/maruel/panicparse/issues/17.
	long := strings.Repeat("a", bufio.MaxScanTokenSize+1)
	data := []string{
		long,
		"panic: reflect.Set: value of type",
		"",
		"goroutine 1 [running]:",
		"github.com/cockroachdb/cockroach/storage/engine._Cfunc_DBIterSeek()",
		" ??:0 +0x6d",
		"gopkg.in/yaml%2ev2.handleErr(0x433b20)",
		"	/gopath/src/gopkg.in/yaml.v2/yaml.go:153 +0xc6",
		"reflect.Value.assignTo(0x570860, 0xc20803f3e0, 0x15)",
		"	/goroot/src/reflect/value.go:2125 +0x368",
		"main.main()",
		"	/gopath/src/github.com/maruel/panicparse/stack/stack.go:428 +0x27",
		"",
	}
	c := NewContext()
	extra := &bytes.Buffer{}
	if err := c.ParseDump(context.Background(), bytes.NewBufferString(strings.Join(data, "\n")), extra); err != nil {
		t.Fatal(err)
	}
	c.GuessPaths()
	compareString(t, long+"\npanic: reflect.Set: value of type\n\n", extra.String())
	want := []*Goroutine{
		{
			Signature: Signature{
				State: "running",
				Stack: Stack{
					Calls: []Call{
						newCall(
							"github.com/cockroachdb/cockroach/storage/engine._Cfunc_DBIterSeek",
							Args{}, "??", 0),
						newCall(
							"gopkg.in/yaml%2ev2.handleErr",
							Args{Values: []Arg{{Value: 0x433b20, IsPtr: true}}},
							"/gopath/src/gopkg.in/yaml.v2/yaml.go",
							153),
						newCall(
							"reflect.Value.assignTo",
							Args{Values: []Arg{{Value: 0x570860, IsPtr: true}, {Value: 0xc20803f3e0, IsPtr: true}, {Value: 0x15}}},
							"/goroot/src/reflect/value.go",
							2125),
						newCall(
							"main.main",
							Args{},
							"/gopath/src/github.com/maruel/panicparse/stack/stack.go",
							428),
					},
				},
			},
			ID:    1,
			First: true,
		},
	}
	for i := range want {
		want[i].updateLocations(c.GOROOT, c.LocalGOROOT, c.GOPATHs)
	}
	compareGoroutines(t, want, c.Goroutines)
}

func TestParseDumpLongWait(t *testing.T) {
	t.Parallel()
	// One call from main, one from stdlib, one from third party.
	data := []string{
		"panic: bleh",
		"",
		"goroutine 1 [chan send, 100 minutes]:",
		"gopkg.in/yaml%2ev2.handleErr(0x433b20)",
		"	/gopath/src/gopkg.in/yaml.v2/yaml.go:153 +0xc6",
		"",
		"goroutine 2 [chan send, locked to thread]:",
		"gopkg.in/yaml%2ev2.handleErr(0x8033b21)",
		"	/gopath/src/gopkg.in/yaml.v2/yaml.go:153 +0xc6",
		"",
		"goroutine 3 [chan send, 101 minutes, locked to thread]:",
		"gopkg.in/yaml%2ev2.handleErr(0x8033b22)",
		"	/gopath/src/gopkg.in/yaml.v2/yaml.go:153 +0xc6",
		"",
	}
	c := NewContext()
	extra := &bytes.Buffer{}
	if err := c.ParseDump(context.Background(), bytes.NewBufferString(strings.Join(data, "\n")), extra); err != nil {
		t.Fatal(err)
	}
	c.GuessPaths()
	compareString(t, "panic: bleh\n\n", extra.String())
	want := []*Goroutine{
		{
			Signature: Signature{
				State:    "chan send",
				SleepMin: 100,
				SleepMax: 100,
				Stack: Stack{
					Calls: []Call{
						newCall(
							"gopkg.in/yaml%2ev2.handleErr",
							Args{Values: []Arg{{Value: 0x433b20, IsPtr: true}}},
							"/gopath/src/gopkg.in/yaml.v2/yaml.go",
							153),
					},
				},
			},
			ID:    1,
			First: true,
		},
		{
			Signature: Signature{
				State:  "chan send",
				Locked: true,
				Stack: Stack{
					Calls: []Call{
						newCall(
							"gopkg.in/yaml%2ev2.handleErr",
							Args{Values: []Arg{{Value: 0x8033b21, Name: "#1", IsPtr: true}}},
							"/gopath/src/gopkg.in/yaml.v2/yaml.go",
							153),
					},
				},
			},
			ID: 2,
		},
		{
			Signature: Signature{
				State:    "chan send",
				SleepMin: 101,
				SleepMax: 101,
				Stack: Stack{
					Calls: []Call{
						newCall(
							"gopkg.in/yaml%2ev2.handleErr",
							Args{Values: []Arg{{Value: 0x8033b22, Name: "#2", IsPtr: true}}},
							"/gopath/src/gopkg.in/yaml.v2/yaml.go",
							153),
					},
				},
				Locked: true,
			},
			ID: 3,
		},
	}
	for i := range want {
		want[i].updateLocations(c.GOROOT, c.LocalGOROOT, c.GOPATHs)
	}
	compareGoroutines(t, want, c.Goroutines)
}

func TestParseDumpAsm(t *testing.T) {
	t.Parallel()
	data := []string{
		"panic: reflect.Set: value of type",
		"",
		"goroutine 16 [garbage collection]:",
		"runtime.switchtoM()",
		"\t/goroot/src/runtime/asm_amd64.s:198 fp=0xc20cfb80d8 sp=0xc20cfb80d0",
		"",
	}
	c := NewContext()
	extra := &bytes.Buffer{}
	if err := c.ParseDump(context.Background(), bytes.NewBufferString(strings.Join(data, "\n")), extra); err != nil {
		t.Fatal(err)
	}
	want := []*Goroutine{
		{
			Signature: Signature{
				State: "garbage collection",
				Stack: Stack{
					Calls: []Call{
						newCall(
							"runtime.switchtoM",
							Args{},
							"/goroot/src/runtime/asm_amd64.s",
							198),
					},
				},
			},
			ID:    16,
			First: true,
		},
	}
	compareGoroutines(t, want, c.Goroutines)
	compareString(t, "panic: reflect.Set: value of type\n\n", extra.String())
}

func TestParseDumpAsmGo1dot13(t *testing.T) {
	t.Parallel()
	data := []string{
		"panic: reflect.Set: value of type",
		"",
		"goroutine 16 [garbage collection]:",
		"runtime.switchtoM()",
		"\t/goroot/src/runtime/asm_amd64.s:198 fp=0xc20cfb80d8 sp=0xc20cfb80d0 pc=0x5007be",
		"",
	}
	c := NewContext()
	extra := &bytes.Buffer{}
	if err := c.ParseDump(context.Background(), bytes.NewBufferString(strings.Join(data, "\n")), extra); err != nil {
		t.Fatal(err)
	}
	want := []*Goroutine{
		{
			Signature: Signature{
				State: "garbage collection",
				Stack: Stack{
					Calls: []Call{
						newCall(
							"runtime.switchtoM",
							Args{},
							"/goroot/src/runtime/asm_amd64.s",
							198),
					},
				},
			},
			ID:    16,
			First: true,
		},
	}
	compareGoroutines(t, want, c.Goroutines)
	compareString(t, "panic: reflect.Set: value of type\n\n", extra.String())
}

func TestParseDumpLineErr(t *testing.T) {
	t.Parallel()
	data := []string{
		"panic: reflect.Set: value of type",
		"",
		"goroutine 1 [running]:",
		"github.com/maruel/panicparse/stack/stack.recurseType()",
		"\t/gopath/src/github.com/maruel/panicparse/stack/stack.go:12345678901234567890",
		"",
	}
	c := NewContext()
	extra := &bytes.Buffer{}
	err := c.ParseDump(context.Background(), bytes.NewBufferString(strings.Join(data, "\n")), extra)
	compareErr(t, errors.New("failed to parse int on line: \"/gopath/src/github.com/maruel/panicparse/stack/stack.go:12345678901234567890\""), err)
	want := []*Goroutine{
		{
			Signature: Signature{
				State: "running",
				Stack: Stack{
					Calls: []Call{
						newCall("github.com/maruel/panicparse/stack/stack.recurseType", Args{}, "", 0),
					},
				},
			},
			ID:    1,
			First: true,
		},
	}
	for i := range want {
		want[i].updateLocations(c.GOROOT, c.LocalGOROOT, c.GOPATHs)
	}
	compareGoroutines(t, want, c.Goroutines)
}

func TestParseDumpCreatedErr(t *testing.T) {
	t.Parallel()
	data := []string{
		"panic: reflect.Set: value of type",
		"",
		"goroutine 1 [running]:",
		"github.com/maruel/panicparse/stack/stack.recurseType()",
		"\t/gopath/src/github.com/maruel/panicparse/stack/stack.go:1",
		"created by testing.RunTests",
		"\t/goroot/src/testing/testing.go:123456789012345678901 +0xa8b",
		"",
	}
	c := NewContext()
	extra := &bytes.Buffer{}
	err := c.ParseDump(context.Background(), bytes.NewBufferString(strings.Join(data, "\n")), extra)
	compareErr(t, errors.New("failed to parse int on line: \"/goroot/src/testing/testing.go:123456789012345678901 +0xa8b\""), err)
	want := []*Goroutine{
		{
			Signature: Signature{
				State:     "running",
				CreatedBy: newCall("testing.RunTests", Args{}, "", 0),
				Stack: Stack{
					Calls: []Call{
						newCall(
							"github.com/maruel/panicparse/stack/stack.recurseType",
							Args{},
							"/gopath/src/github.com/maruel/panicparse/stack/stack.go",
							1),
					},
				},
			},
			ID:    1,
			First: true,
		},
	}
	compareGoroutines(t, want, c.Goroutines)
}

func TestParseDumpValueErr(t *testing.T) {
	t.Parallel()
	data := []string{
		"panic: reflect.Set: value of type",
		"",
		"goroutine 1 [running]:",
		"github.com/maruel/panicparse/stack/stack.recurseType(123456789012345678901)",
		"\t/gopath/src/github.com/maruel/panicparse/stack/stack.go:9",
		"",
	}
	c := NewContext()
	extra := &bytes.Buffer{}
	err := c.ParseDump(context.Background(), bytes.NewBufferString(strings.Join(data, "\n")), extra)
	compareErr(t, errors.New("failed to parse int on line: \"github.com/maruel/panicparse/stack/stack.recurseType(123456789012345678901)\""), err)
	want := []*Goroutine{
		{
			Signature: Signature{
				State: "running",
				Stack: Stack{
					Calls: []Call{
						newCall("github.com/maruel/panicparse/stack/stack.recurseType", Args{}, "", 0),
					},
				},
			},
			ID:    1,
			First: true,
		},
	}
	for i := range want {
		want[i].updateLocations(c.GOROOT, c.LocalGOROOT, c.GOPATHs)
	}
	compareGoroutines(t, want, c.Goroutines)
}

func TestParseDumpInconsistentIndent(t *testing.T) {
	t.Parallel()
	data := []string{
		"  goroutine 1 [running]:",
		"  github.com/maruel/panicparse/stack/stack.recurseType()",
		" \t/gopath/src/github.com/maruel/panicparse/stack/stack.go:1",
		"",
	}
	c := NewContext()
	extra := &bytes.Buffer{}
	err := c.ParseDump(context.Background(), bytes.NewBufferString(strings.Join(data, "\n")), extra)
	compareErr(t, errors.New(`inconsistent indentation: " \t/gopath/src/github.com/maruel/panicparse/stack/stack.go:1", expected "  "`), err)
	want := []*Goroutine{
		{
			Signature: Signature{
				State: "running",
				Stack: Stack{
					Calls: []Call{
						newCall("github.com/maruel/panicparse/stack/stack.recurseType", Args{}, "", 0),
					},
				},
			},
			ID:    1,
			First: true,
		},
	}
	compareGoroutines(t, want, c.Goroutines)
	compareString(t, "", extra.String())
}

func TestParseDumpOrderErr(t *testing.T) {
	t.Parallel()
	data := []string{
		"panic: reflect.Set: value of type",
		"",
		"goroutine 16 [garbage collection]:",
		"	/gopath/src/gopkg.in/yaml.v2/yaml.go:153 +0xc6",
		"runtime.switchtoM()",
		"\t/goroot/src/runtime/asm_amd64.s:198 fp=0xc20cfb80d8 sp=0xc20cfb80d0",
		"",
	}
	c := NewContext()
	extra := &bytes.Buffer{}
	err := c.ParseDump(context.Background(), bytes.NewBufferString(strings.Join(data, "\n")), extra)
	compareErr(t, errors.New("expected a function after a goroutine header, got: \"/gopath/src/gopkg.in/yaml.v2/yaml.go:153 +0xc6\""), err)
	want := []*Goroutine{
		{
			Signature: Signature{State: "garbage collection"},
			ID:        16,
			First:     true,
		},
	}
	compareGoroutines(t, want, c.Goroutines)
	compareString(t, "panic: reflect.Set: value of type\n\n", extra.String())
}

func TestParseDumpElided(t *testing.T) {
	t.Parallel()
	data := []string{
		"panic: reflect.Set: value of type",
		"",
		"goroutine 16 [garbage collection]:",
		"github.com/maruel/panicparse/stack/stack.recurseType(0x7f4fa9a3ec70, 0xc208062580, 0x7f4fa9a3e818, 0x50a820, 0xc20803a8a0)",
		"\t/gopath/src/github.com/maruel/panicparse/stack/stack.go:53 +0x845 fp=0xc20cfc66d8 sp=0xc20cfc6470",
		"...additional frames elided...",
		"created by testing.RunTests",
		"\t/goroot/src/testing/testing.go:555 +0xa8b",
		"",
	}
	extra := &bytes.Buffer{}
	c := NewContext()
	if err := c.ParseDump(context.Background(), bytes.NewBufferString(strings.Join(data, "\n")), extra); err != nil {
		t.Fatal(err)
	}
	want := []*Goroutine{
		{
			Signature: Signature{
				State: "garbage collection",
				CreatedBy: newCall(
					"testing.RunTests",
					Args{},
					"/goroot/src/testing/testing.go",
					555),
				Stack: Stack{
					Calls: []Call{
						newCall(
							"github.com/maruel/panicparse/stack/stack.recurseType",
							Args{
								Values: []Arg{
									{Value: 0x7f4fa9a3ec70, IsPtr: true},
									{Value: 0xc208062580, IsPtr: true},
									{Value: 0x7f4fa9a3e818, IsPtr: true},
									{Value: 0x50a820, IsPtr: true},
									{Value: 0xc20803a8a0, IsPtr: true},
								},
							},
							"/gopath/src/github.com/maruel/panicparse/stack/stack.go",
							53),
					},
					Elided: true,
				},
			},
			ID:    16,
			First: true,
		},
	}
	compareGoroutines(t, want, c.Goroutines)
	compareString(t, "panic: reflect.Set: value of type\n\n", extra.String())
}

func TestParseDumpSysCall(t *testing.T) {
	t.Parallel()
	data := []string{
		"panic: reflect.Set: value of type",
		"",
		"goroutine 5 [syscall]:",
		"runtime.notetsleepg(0x918100, 0xffffffffffffffff, 0x1)",
		"\t/goroot/src/runtime/lock_futex.go:201 +0x52 fp=0xc208018f68 sp=0xc208018f40",
		"runtime.signal_recv(0x0)",
		"\t/goroot/src/runtime/sigqueue.go:109 +0x135 fp=0xc208018fa0 sp=0xc208018f68",
		"os/signal.loop()",
		"\t/goroot/src/os/signal/signal_unix.go:21 +0x1f fp=0xc208018fe0 sp=0xc208018fa0",
		"runtime.goexit()",
		"\t/goroot/src/runtime/asm_amd64.s:2232 +0x1 fp=0xc208018fe8 sp=0xc208018fe0",
		"created by os/signal.init·1",
		"\t/goroot/src/os/signal/signal_unix.go:27 +0x35",
		"",
	}
	c := NewContext()
	extra := &bytes.Buffer{}
	if err := c.ParseDump(context.Background(), bytes.NewBufferString(strings.Join(data, "\n")), extra); err != nil {
		t.Fatal(err)
	}
	want := []*Goroutine{
		{
			Signature: Signature{
				State: "syscall",
				CreatedBy: newCall(
					"os/signal.init·1",
					Args{},
					"/goroot/src/os/signal/signal_unix.go",
					27),
				Stack: Stack{
					Calls: []Call{
						newCall(
							"runtime.notetsleepg",
							Args{
								Values: []Arg{
									{Value: 0x918100, IsPtr: true},
									{Value: 0xffffffffffffffff},
									{Value: 0x1},
								},
							},
							"/goroot/src/runtime/lock_futex.go",
							201),
						newCall(
							"runtime.signal_recv",
							Args{Values: []Arg{{}}},
							"/goroot/src/runtime/sigqueue.go",
							109),
						newCall(
							"os/signal.loop",
							Args{},
							"/goroot/src/os/signal/signal_unix.go",
							21),
						newCall(
							"runtime.goexit",
							Args{},
							"/goroot/src/runtime/asm_amd64.s",
							2232),
					},
				},
			},
			ID:    5,
			First: true,
		},
	}
	compareGoroutines(t, want, c.Goroutines)
	compareString(t, "panic: reflect.Set: value of type\n\n", extra.String())
}

func TestParseDumpUnavailCreated(t *testing.T) {
	t.Parallel()
	data := []string{
		"panic: reflect.Set: value of type",
		"",
		"goroutine 24 [running]:",
		"\tgoroutine running on other thread; stack unavailable",
		"created by github.com/maruel/panicparse/stack.New",
		"\t/gopath/src/github.com/maruel/panicparse/stack/stack.go:131 +0x381",
		"",
	}
	c := NewContext()
	extra := &bytes.Buffer{}
	if err := c.ParseDump(context.Background(), bytes.NewBufferString(strings.Join(data, "\n")), extra); err != nil {
		t.Fatal(err)
	}
	want := []*Goroutine{
		{
			Signature: Signature{
				State: "running",
				CreatedBy: newCall(
					"github.com/maruel/panicparse/stack.New",
					Args{},
					"/gopath/src/github.com/maruel/panicparse/stack/stack.go",
					131),
				Stack: Stack{
					Calls: []Call{newCall("", Args{}, "<unavailable>", 0)},
				},
			},
			ID:    24,
			First: true,
		},
	}
	compareGoroutines(t, want, c.Goroutines)
	compareString(t, "panic: reflect.Set: value of type\n\n", extra.String())
}

func TestParseDumpUnavail(t *testing.T) {
	t.Parallel()
	data := []string{
		"panic: reflect.Set: value of type",
		"",
		"goroutine 24 [running]:",
		"\tgoroutine running on other thread; stack unavailable",
		"",
		"",
	}
	c := NewContext()
	extra := &bytes.Buffer{}
	if err := c.ParseDump(context.Background(), bytes.NewBufferString(strings.Join(data, "\n")), extra); err != nil {
		t.Fatal(err)
	}
	want := []*Goroutine{
		{
			Signature: Signature{
				State: "running",
				Stack: Stack{
					Calls: []Call{newCall("", Args{}, "<unavailable>", 0)},
				},
			},
			ID:    24,
			First: true,
		},
	}
	compareGoroutines(t, want, c.Goroutines)
	compareString(t, "panic: reflect.Set: value of type\n\n", extra.String())
}

func TestParseDumpUnavailError(t *testing.T) {
	t.Parallel()
	data := []string{
		"panic: reflect.Set: value of type",
		"",
		"goroutine 24 [running]:",
		"\tgoroutine running on other thread; stack unavailable",
		"junk",
	}
	c := NewContext()
	extra := &bytes.Buffer{}
	err := c.ParseDump(context.Background(), bytes.NewBufferString(strings.Join(data, "\n")), extra)
	compareErr(t, errors.New("expected empty line after unavailable stack, got: \"junk\""), err)
	want := []*Goroutine{
		{
			Signature: Signature{
				State: "running",
				Stack: Stack{
					Calls: []Call{newCall("", Args{}, "<unavailable>", 0)},
				},
			},
			ID:    24,
			First: true,
		},
	}
	compareGoroutines(t, want, c.Goroutines)
	compareString(t, "panic: reflect.Set: value of type\n\n", extra.String())
}

func TestParseDumpNoOffset(t *testing.T) {
	t.Parallel()
	data := []string{
		"panic: runtime error: index out of range",
		"",
		"goroutine 37 [runnable]:",
		"github.com/maruel/panicparse/stack.func·002()",
		"	/gopath/src/github.com/maruel/panicparse/stack/stack.go:110",
		"created by github.com/maruel/panicparse/stack.New",
		"	/gopath/src/github.com/maruel/panicparse/stack/stack.go:113 +0x43b",
		"",
	}
	c := NewContext()
	if err := c.ParseDump(context.Background(), bytes.NewBufferString(strings.Join(data, "\n")), ioutil.Discard); err != nil {
		t.Fatal(err)
	}
	wantGR := []*Goroutine{
		{
			Signature: Signature{
				State: "runnable",
				CreatedBy: newCall(
					"github.com/maruel/panicparse/stack.New",
					Args{},
					"/gopath/src/github.com/maruel/panicparse/stack/stack.go",
					113),
				Stack: Stack{
					Calls: []Call{
						newCall(
							"github.com/maruel/panicparse/stack.func·002",
							Args{},
							"/gopath/src/github.com/maruel/panicparse/stack/stack.go",
							110),
					},
				},
			},
			ID:    37,
			First: true,
		},
	}
	compareGoroutines(t, wantGR, c.Goroutines)
}

func TestParseDumpHeaderError(t *testing.T) {
	t.Parallel()
	// For coverage of scanLines.
	data := []string{
		"panic: reflect.Set: value of type",
		"",
		"goroutine 1 [running]:",
		"junk",
	}
	c := NewContext()
	extra := &bytes.Buffer{}
	err := c.ParseDump(context.Background(), bytes.NewBufferString(strings.Join(data, "\n")), extra)
	compareErr(t, errors.New("expected a function after a goroutine header, got: \"junk\""), err)
	want := []*Goroutine{
		{
			Signature: Signature{State: "running"},
			ID:        1,
			First:     true,
		},
	}
	compareGoroutines(t, want, c.Goroutines)
	compareString(t, "panic: reflect.Set: value of type\n\n", extra.String())
}

func TestParseDumpFileError(t *testing.T) {
	t.Parallel()
	// For coverage of scanLines.
	data := []string{
		"panic: reflect.Set: value of type",
		"",
		"goroutine 1 [running]:",
		"github.com/maruel/panicparse/stack.func·002()",
		"junk",
	}
	c := NewContext()
	extra := &bytes.Buffer{}
	err := c.ParseDump(context.Background(), bytes.NewBufferString(strings.Join(data, "\n")), extra)
	compareErr(t, errors.New("expected a file after a function, got: \"junk\""), err)
	want := []*Goroutine{
		{
			Signature: Signature{
				State: "running",
				Stack: Stack{
					Calls: []Call{
						newCall("github.com/maruel/panicparse/stack.func·002", Args{}, "", 0),
					},
				},
			},
			ID:    1,
			First: true,
		},
	}
	compareGoroutines(t, want, c.Goroutines)
	compareString(t, "panic: reflect.Set: value of type\n\n", extra.String())
}

func TestParseDumpCreated(t *testing.T) {
	t.Parallel()
	// For coverage of scanLines.
	data := []string{
		"panic: reflect.Set: value of type",
		"",
		"goroutine 1 [running]:",
		"github.com/maruel/panicparse/stack.func·002()",
		"	/gopath/src/github.com/maruel/panicparse/stack/stack.go:110",
		"created by github.com/maruel/panicparse/stack.New",
		"\t/gopath/src/github.com/maruel/panicparse/stack/stack.go:131 +0x381",
		"exit status 2",
	}
	c := NewContext()
	extra := &bytes.Buffer{}
	if err := c.ParseDump(context.Background(), bytes.NewBufferString(strings.Join(data, "\n")), extra); err != nil {
		t.Fatal(err)
	}
	want := []*Goroutine{
		{
			Signature: Signature{
				State: "running",
				CreatedBy: newCall(
					"github.com/maruel/panicparse/stack.New",
					Args{},
					"/gopath/src/github.com/maruel/panicparse/stack/stack.go",
					131),
				Stack: Stack{
					Calls: []Call{
						newCall(
							"github.com/maruel/panicparse/stack.func·002",
							Args{},
							"/gopath/src/github.com/maruel/panicparse/stack/stack.go",
							110),
					},
				},
			},
			ID:    1,
			First: true,
		},
	}
	compareGoroutines(t, want, c.Goroutines)
	compareString(t, "panic: reflect.Set: value of type\n\nexit status 2", extra.String())
}

func TestParseDumpCreatedError(t *testing.T) {
	t.Parallel()
	// For coverage of scanLines.
	data := []string{
		"panic: reflect.Set: value of type",
		"",
		"goroutine 1 [running]:",
		"github.com/maruel/panicparse/stack.func·002()",
		"	/gopath/src/github.com/maruel/panicparse/stack/stack.go:110",
		"created by github.com/maruel/panicparse/stack.New",
		"junk",
	}
	c := NewContext()
	extra := &bytes.Buffer{}
	err := c.ParseDump(context.Background(), bytes.NewBufferString(strings.Join(data, "\n")), extra)
	compareErr(t, errors.New("expected a file after a created line, got: \"junk\""), err)
	want := []*Goroutine{
		{
			Signature: Signature{
				State:     "running",
				CreatedBy: newCall("github.com/maruel/panicparse/stack.New", Args{}, "", 0),
				Stack: Stack{
					Calls: []Call{
						newCall(
							"github.com/maruel/panicparse/stack.func·002",
							Args{},
							"/gopath/src/github.com/maruel/panicparse/stack/stack.go",
							110),
					},
				},
			},
			ID:    1,
			First: true,
		},
	}
	compareGoroutines(t, want, c.Goroutines)
	compareString(t, "panic: reflect.Set: value of type\n\n", extra.String())
}

func TestParseDumpCCode(t *testing.T) {
	t.Parallel()
	data := []string{
		"SIGQUIT: quit",
		"PC=0x43f349",
		"",
		"goroutine 0 [idle]:",
		"runtime.epollwait(0x4, 0x7fff671c7118, 0xffffffff00000080, 0x0, 0xffffffff0028c1be, 0x0, 0x0, 0x0, 0x0, 0x0, ...)",
		"        /goroot/src/runtime/sys_linux_amd64.s:400 +0x19",
		"runtime.netpoll(0x901b01, 0x0)",
		"        /goroot/src/runtime/netpoll_epoll.go:68 +0xa3",
		"findrunnable(0xc208012000)",
		"        /goroot/src/runtime/proc.c:1472 +0x485",
		"schedule()",
		"        /goroot/src/runtime/proc.c:1575 +0x151",
		"runtime.park_m(0xc2080017a0)",
		"        /goroot/src/runtime/proc.c:1654 +0x113",
		"runtime.mcall(0x432684)",
		"        /goroot/src/runtime/asm_amd64.s:186 +0x5a",
		"",
	}
	c := NewContext()
	if err := c.ParseDump(context.Background(), bytes.NewBufferString(strings.Join(data, "\n")), ioutil.Discard); err != nil {
		t.Fatal(err)
	}
	wantGR := []*Goroutine{
		{
			Signature: Signature{
				State: "idle",
				Stack: Stack{
					Calls: []Call{
						newCall(
							"runtime.epollwait",
							Args{
								Values: []Arg{
									{Value: 0x4},
									{Value: 0x7fff671c7118, IsPtr: true},
									{Value: 0xffffffff00000080},
									{},
									{Value: 0xffffffff0028c1be},
									{},
									{},
									{},
									{},
									{},
								},
								Elided: true,
							},
							"/goroot/src/runtime/sys_linux_amd64.s",
							400),
						newCall(
							"runtime.netpoll",
							Args{Values: []Arg{{Value: 0x901b01, IsPtr: true}, {}}},
							"/goroot/src/runtime/netpoll_epoll.go",
							68),
						newCall(
							"findrunnable",
							Args{Values: []Arg{{Value: 0xc208012000, IsPtr: true}}},
							"/goroot/src/runtime/proc.c",
							1472),
						newCall("schedule", Args{}, "/goroot/src/runtime/proc.c", 1575),
						newCall(
							"runtime.park_m",
							Args{Values: []Arg{{Value: 0xc2080017a0, IsPtr: true}}},
							"/goroot/src/runtime/proc.c",
							1654),
						newCall(
							"runtime.mcall",
							Args{Values: []Arg{{Value: 0x432684, IsPtr: true}}},
							"/goroot/src/runtime/asm_amd64.s",
							186),
					},
				},
			},
			ID:    0,
			First: true,
		},
	}
	compareGoroutines(t, wantGR, c.Goroutines)
}

func TestParseDumpWithCarriageReturn(t *testing.T) {
	t.Parallel()
	data := []string{
		"goroutine 1 [running]:",
		"github.com/cockroachdb/cockroach/storage/engine._Cfunc_DBIterSeek()",
		" ??:0 +0x6d",
		"gopkg.in/yaml%2ev2.handleErr(0x433b20)",
		"	/gopath/src/gopkg.in/yaml.v2/yaml.go:153 +0xc6",
		"reflect.Value.assignTo(0x570860, 0xc20803f3e0, 0x15)",
		"	/goroot/src/reflect/value.go:2125 +0x368",
		"main.main()",
		"	/gopath/src/github.com/maruel/panicparse/stack/stack.go:428 +0x27",
		"",
	}
	c := NewContext()
	if err := c.ParseDump(context.Background(), bytes.NewBufferString(strings.Join(data, "\r\n")), ioutil.Discard); err != nil {
		t.Fatal(err)
	}
	want := []*Goroutine{
		{
			Signature: Signature{
				State: "running",
				Stack: Stack{
					Calls: []Call{
						newCall(
							"github.com/cockroachdb/cockroach/storage/engine._Cfunc_DBIterSeek",
							Args{},
							"??",
							0),
						newCall(
							"gopkg.in/yaml%2ev2.handleErr",
							Args{Values: []Arg{{Value: 0x433b20, IsPtr: true}}},
							"/gopath/src/gopkg.in/yaml.v2/yaml.go",
							153),
						newCall(
							"reflect.Value.assignTo",
							Args{Values: []Arg{{Value: 0x570860, IsPtr: true}, {Value: 0xc20803f3e0, IsPtr: true}, {Value: 0x15}}},
							"/goroot/src/reflect/value.go",
							2125),
						newCall(
							"main.main",
							Args{},
							"/gopath/src/github.com/maruel/panicparse/stack/stack.go",
							428),
					},
				},
			},
			ID:    1,
			First: true,
		},
	}
	compareGoroutines(t, want, c.Goroutines)
}

func TestParseDumpIndented(t *testing.T) {
	t.Parallel()
	// goconvey is culprit of this.
	data := []string{
		"Failures:",
		"",
		"  * /home/maruel/go/src/foo/bar_test.go",
		"  Line 209:",
		"  Expected: '(*errors.errorString){s:\"context canceled\"}'",
		"  Actual:   'nil'",
		"  (Should resemble)!",
		"  goroutine 8 [running]:",
		"  foo/bar.TestArchiveFail.func1.2()",
		"        /home/maruel/go/foo/bar_test.go:209 +0x469",
		"  foo/bar.TestArchiveFail(0x3382000)",
		"        /home/maruel/go/src/foo/bar_test.go:155 +0xf1",
		"  testing.tRunner(0x3382000, 0x1615bf8)",
		"        /home/maruel/golang/go/src/testing/testing.go:865 +0xc0",
		"  created by testing.(*T).Run",
		"        /home/maruel/golang/go/src/testing/testing.go:916 +0x35a",
		"",
		"",
	}
	c := NewContext()
	extra := bytes.Buffer{}
	if err := c.ParseDump(context.Background(), bytes.NewBufferString(strings.Join(data, "\n")), &extra); err != nil {
		t.Fatal(err)
	}
	compareString(t, strings.Join(data[:7], "\n")+"\n", extra.String())
	want := []*Goroutine{
		{
			Signature: Signature{
				State: "running",
				CreatedBy: newCall(
					"testing.(*T).Run",
					Args{},
					"/home/maruel/golang/go/src/testing/testing.go",
					916),
				Stack: Stack{
					Calls: []Call{
						newCall(
							"foo/bar.TestArchiveFail.func1.2",
							Args{},
							"/home/maruel/go/foo/bar_test.go",
							209),
						newCall(
							"foo/bar.TestArchiveFail",
							Args{Values: []Arg{{Value: 0x3382000, Name: "#1", IsPtr: true}}},
							"/home/maruel/go/src/foo/bar_test.go",
							155),
						newCall(
							"testing.tRunner",
							Args{Values: []Arg{{Value: 0x3382000, Name: "#1", IsPtr: true}, {Value: 0x1615bf8, IsPtr: true}}},
							"/home/maruel/golang/go/src/testing/testing.go",
							865),
					},
				},
			},
			ID:    8,
			First: true,
		},
	}
	compareGoroutines(t, want, c.Goroutines)
}

func TestParseDumpRace(t *testing.T) {
	t.Parallel()
	// Generated with "panic race":
	data := []string{
		"==================",
		"WARNING: DATA RACE",
		"Read at 0x00c0000e4030 by goroutine 7:",
		"  main.panicRace.func1()",
		"      /go/src/github.com/maruel/panicparse/cmd/panic/main_race.go:37 +0x38",
		"",
		"Previous write at 0x00c0000e4030 by goroutine 6:",
		"  main.panicRace.func1()",
		"      /go/src/github.com/maruel/panicparse/cmd/panic/main_race.go:37 +0x4e",
		"",
		"Goroutine 7 (running) created at:",
		"  main.panicRace()",
		"      /go/src/github.com/maruel/panicparse/cmd/panic/main_race.go:35 +0x88",
		"  main.main()",
		"      /go/src/github.com/maruel/panicparse/cmd/panic/main.go:252 +0x2d9",
		"",
		"Goroutine 6 (running) created at:",
		"  main.panicRace()",
		"      /go/src/github.com/maruel/panicparse/cmd/panic/main_race.go:35 +0x88",
		"  main.main()",
		"      /go/src/github.com/maruel/panicparse/cmd/panic/main.go:252 +0x2d9",
		"==================",
		"",
	}
	c := NewContext()
	extra := &bytes.Buffer{}
	if err := c.ParseDump(context.Background(), bytes.NewBufferString(strings.Join(data, "\n")), extra); err != nil {
		t.Fatal(err)
	}
	want := []*Goroutine{
		{
			Signature: Signature{
				State: "running",
				Stack: Stack{
					Calls: []Call{
						newCall(
							"main.panicRace",
							Args{},
							"/go/src/github.com/maruel/panicparse/cmd/panic/main.go",
							252),
					},
				},
			},
			ID:    7,
			First: true,
		},
		{
			Signature: Signature{
				State: "running",
				Stack: Stack{
					Calls: []Call{
						newCall(
							"main.panicRace",
							Args{},
							"/go/src/github.com/maruel/panicparse/cmd/panic/main.go",
							252),
					},
				},
			},
			ID: 6,
		},
	}
	compareGoroutines(t, want, c.Goroutines)
	wantOps := []RaceOp{{Write: false, Addr: 0xc0000e4030, ID: 7}, {Write: true, Addr: 0xc0000e4030, ID: 6}}
	if diff := cmp.Diff(wantOps, c.Races); diff != "" {
		t.Fatalf("%s", diff)
	}
}

func TestSplitPath(t *testing.T) {
	t.Parallel()
	if p := splitPath(""); p != nil {
		t.Fatalf("expected nil, got: %v", p)
	}
}

func TestGetGOPATHS(t *testing.T) {
	old := os.Getenv("GOPATH")
	defer func() {
		os.Setenv("GOPATH", old)
	}()
	os.Setenv("GOPATH", "")
	if p := getGOPATHs(); len(p) != 1 {
		t.Fatalf("expected only one path: %v", p)
	}
}

// Test runtime code. For now just assert that they succeed (beside race).
// Later they'll be used for the actual expectations instead of the hardcoded
// ones above.
func TestPanic(t *testing.T) {
	t.Parallel()
	cmds := internaltest.PanicOutputs()
	want := map[string]int{
		"chan_receive":              2,
		"chan_send":                 2,
		"goroutine_1":               2,
		"goroutine_dedupe_pointers": 101,
		"goroutine_100":             101,
	}

	panicParseDir := getPanicParseDir(t)
	ppDir := pathJoin(panicParseDir, "cmd", "panic")

	custom := map[string]func(*testing.T, *Context, *bytes.Buffer, string){
		"args_elided": testPanicArgsElided,
		"mismatched":  testPanicMismatched,
		"race":        testPanicRace,
		"str":         testPanicStr,
		"utf8":        testPanicUTF8,
	}
	// Make sure all custom handlers are showing up in cmds.
	for n := range custom {
		if _, ok := cmds[n]; !ok {
			t.Fatalf("untested mode: %q in:\n%v", n, cmds)
		}
	}

	for cmd, data := range cmds {
		cmd := cmd
		data := data
		t.Run(cmd, func(t *testing.T) {
			t.Parallel()
			b := bytes.Buffer{}
			c := NewContext()
			if err := c.ParseDump(context.Background(), bytes.NewReader(data), &b); err != nil {
				t.Fatal(err)
			}
			c.GuessPaths()

			if c.Goroutines == nil {
				t.Fatal("context is nil")
			}
			if f := custom[cmd]; f != nil {
				f(t, c, &b, ppDir)
				return
			}
			if c.Races != nil {
				t.Fatal("unexpected race")
			}
			if c.GOROOT != runtime.GOROOT() {
				//t.Logf("GOROOT is %q", c.GOROOT)
			}
			e := want[cmd]
			if e == 0 {
				e = 1
			}
			if got := len(c.Goroutines); got != e {
				t.Fatalf("unexpected Goroutines; want %d, got %d", e, got)
			}
		})
	}
}

func testPanicArgsElided(t *testing.T, c *Context, b *bytes.Buffer, ppDir string) {
	if c.GOROOT != "" {
		t.Fatalf("GOROOT is %q", c.GOROOT)
	}
	if b.String() != "GOTRACEBACK=all\npanic: 1\n\n" {
		t.Fatalf("output: %q", b.String())
	}
	want := []*Goroutine{
		{
			Signature: Signature{
				State: "running",
				Stack: Stack{
					Calls: []Call{
						newCallLocal(
							"main.panicArgsElided",
							Args{
								Values: []Arg{{Value: 1}, {Value: 2}, {Value: 3}, {Value: 4}, {Value: 5}, {Value: 6}, {Value: 7}, {Value: 8}, {Value: 9}, {Value: 10}},
								Elided: true,
							},
							pathJoin(ppDir, "main.go"),
							58),
						newCallLocal("main.glob..func1", Args{}, pathJoin(ppDir, "main.go"), 134),
						newCallLocal("main.main", Args{}, pathJoin(ppDir, "main.go"), 340),
					},
				},
			},
			ID:    1,
			First: true,
		},
	}
	similarGoroutines(t, want, c.Goroutines)
}

func testPanicMismatched(t *testing.T, c *Context, b *bytes.Buffer, ppDir string) {
	if c.GOROOT != "" {
		t.Fatalf("GOROOT is %q", c.GOROOT)
	}
	if b.String() != "GOTRACEBACK=all\npanic: 42\n\n" {
		t.Fatalf("output: %q", b.String())
	}
	ver := "/v2"
	if !internaltest.IsUsingModules() {
		ver = ""
	}
	want := []*Goroutine{
		{
			Signature: Signature{
				State: "running",
				Stack: Stack{
					Calls: []Call{
						newCallLocal(
							// This is important to note here that the Go runtime prints out
							// the package path, and not the package name.
							//
							// Here the package name is "correct". There is no way to deduce
							// this from the stack trace.
							"github.com/maruel/panicparse"+ver+"/cmd/panic/internal/incorrect.Panic",
							Args{},
							pathJoin(ppDir, "internal", "incorrect", "correct.go"),
							7),
						newCallLocal("main.glob..func18", Args{}, pathJoin(ppDir, "main.go"), 314),
						newCallLocal("main.main", Args{}, pathJoin(ppDir, "main.go"), 340),
					},
				},
			},
			ID:    1,
			First: true,
		},
	}
	similarGoroutines(t, want, c.Goroutines)
}

func testPanicRace(t *testing.T, c *Context, b *bytes.Buffer, ppDir string) {
	if c.GOROOT != "" {
		t.Fatalf("GOROOT is %q", c.GOROOT)
	}
	if b.String() != "GOTRACEBACK=all\n" {
		t.Fatalf("output: %q", b.String())
	}
	want := []*Goroutine{
		{
			Signature: Signature{
				State: "running",
				Stack: Stack{
					Calls: []Call{
						{
							Func: Func{
								Complete:   "main.panicRaceEnabled",
								ImportPath: "main",
								DirName:    "main",
								Name:       "panicRaceEnabled",
								IsPkgMain:  true,
							},
							SrcPath:      pathJoin(ppDir, "main.go"),
							Line:         54,
							SrcName:      "main.go",
							DirSrc:       "panic/main.go",
							LocalSrcPath: pathJoin(ppDir, "main.go"),
							RelSrcPath:   "github.com/maruel/panicparse/cmd/panic/main.go",
						},
					},
				},
			},
			ID:    1,
			First: true,
		},
		{
			Signature: Signature{
				State: "running",
				Stack: Stack{
					Calls: []Call{
						{
							Func: Func{
								Complete:   "main.panicRaceEnabled",
								ImportPath: "main",
								DirName:    "main",
								Name:       "panicRaceEnabled",
								IsPkgMain:  true,
							},
							SrcPath:      pathJoin(ppDir, "main.go"),
							Line:         54,
							SrcName:      "main.go",
							DirSrc:       "panic/main.go",
							LocalSrcPath: pathJoin(ppDir, "main.go"),
							RelSrcPath:   "github.com/maruel/panicparse/cmd/panic/main.go",
						},
					},
				},
			},
			ID: 2,
		},
	}
	// IDs are not deterministic, so zap them too but take them for the race
	// detector first.
	wantOps := []RaceOp{
		{Addr: pointer, ID: c.Goroutines[0].ID},
		{Write: true, Addr: pointer, ID: c.Goroutines[1].ID},
	}
	for i := range c.Goroutines {
		c.Goroutines[i].ID = i + 1
	}
	similarGoroutines(t, want, c.Goroutines)

	for i := range c.Races {
		if c.Races[i].Addr > 4*1024*1024 {
			c.Races[i].Addr = pointer
		}
	}
	if diff := cmp.Diff(c.Races, wantOps); diff != "" {
		t.Fatal(diff)
	}
}

func testPanicStr(t *testing.T, c *Context, b *bytes.Buffer, ppDir string) {
	if c.GOROOT != "" {
		t.Fatalf("GOROOT is %q", c.GOROOT)
	}
	if b.String() != "GOTRACEBACK=all\npanic: allo\n\n" {
		t.Fatalf("output: %q", b.String())
	}
	want := []*Goroutine{
		{
			Signature: Signature{
				State: "running",
				Stack: Stack{
					Calls: []Call{
						newCallLocal(
							"main.panicstr",
							Args{Values: []Arg{{Value: 0x123456, IsPtr: true}, {Value: 4}}},
							pathJoin(ppDir, "main.go"),
							50),
						newCallLocal("main.glob..func17", Args{}, pathJoin(ppDir, "main.go"), 307),
						newCallLocal("main.main", Args{}, pathJoin(ppDir, "main.go"), 340),
					},
				},
			},
			ID:    1,
			First: true,
		},
	}
	similarGoroutines(t, want, c.Goroutines)
}

func testPanicUTF8(t *testing.T, c *Context, b *bytes.Buffer, ppDir string) {
	if c.GOROOT != "" {
		t.Fatalf("GOROOT is %q", c.GOROOT)
	}
	if b.String() != "GOTRACEBACK=all\npanic: 42\n\n" {
		t.Fatalf("output: %q", b.String())
	}
	ver := "/v2"
	if !internaltest.IsUsingModules() {
		ver = ""
	}
	want := []*Goroutine{
		{
			Signature: Signature{
				State: "running",
				Stack: Stack{
					Calls: []Call{
						newCallLocal(
							// This is important to note here the inconsistency in the Go
							// runtime stack generator. The path is escaped, but symbols are
							// not.
							"github.com/maruel/panicparse"+ver+"/cmd/panic/internal/%c3%b9tf8.(*Strùct).Pànic",
							Args{Values: []Arg{{Value: 0xc0000b2e48, IsPtr: true}}},
							// See TestCallUTF8 in stack_test.go for exercising the methods on
							// Call in this situation.
							pathJoin(ppDir, "internal", "ùtf8", "ùtf8.go"),
							10),
						newCallLocal("main.glob..func19", Args{}, pathJoin(ppDir, "main.go"), 322),
						newCallLocal("main.main", Args{}, pathJoin(ppDir, "main.go"), 340),
					},
				},
			},
			ID:    1,
			First: true,
		},
	}
	similarGoroutines(t, want, c.Goroutines)
}

// TestPanicweb implements the parsing of panicweb output.
//
// panicweb is a separate binary from the rest of panic because importing the
// "net" package causes a background thread to be started, which breaks "panic
// asleep".
func TestPanicweb(t *testing.T) {
	t.Parallel()
	c := NewContext()
	b := bytes.Buffer{}
	if err := c.ParseDump(context.Background(), bytes.NewReader(internaltest.PanicwebOutput()), &b); err != nil {
		t.Fatal(err)
	}
	c.GuessPaths()
	if c.Goroutines == nil {
		t.Fatal("context is nil")
	}
	if b.String() != "panic: Here's a snapshot of a normal web server.\n\n" {
		t.Fatalf("output: %q", b.String())
	}
	if c.GOROOT != strings.Replace(runtime.GOROOT(), "\\", "/", -1) {
		t.Fatalf("GOROOT mismatch; want:%q got:%q", runtime.GOROOT(), c.GOROOT)
	}
	if got := len(c.Goroutines); got < 30 {
		t.Fatalf("unexpected Goroutines; want at least 30, got %d", got)
	}
	// Reduce the goroutines.
	got := Aggregate(c.Goroutines, AnyPointer)
	// The goal here is not to find the exact match since it'll change across
	// OSes and Go versions, but to find some of the expected signatures.
	pwebDir := pathJoin(getPanicParseDir(t), "cmd", "panicweb")
	// Categorize the signatures.
	var types []panicwebSignatureType
	for _, b := range got {
		types = append(types, identifyPanicwebSignature(t, b, pwebDir))
	}
	// Count the expected types.
	if v := pstCount(types, pstUnknown); v != 0 {
		t.Fatalf("found %d unknown signatures", v)
	}
	if v := pstCount(types, pstMain); v != 1 {
		t.Fatalf("found %d pstMain signatures", v)
	}
	if v := pstCount(types, pstURL1handler); v != 1 {
		t.Fatalf("found %d URL1Handler signatures", v)
	}
	if v := pstCount(types, pstURL2handler); v != 1 {
		t.Fatalf("found %d URL2Handler signatures", v)
	}
	if v := pstCount(types, pstClient); v == 0 {
		t.Fatalf("found %d client signatures", v)
	}
	if v := pstCount(types, pstServe); v != 1 {
		t.Fatalf("found %d serve signatures", v)
	}
	if v := pstCount(types, pstColorable); v != 1 {
		t.Fatalf("found %d colorable signatures", v)
	}
	if v := pstCount(types, pstStdlib); v < 3 {
		t.Fatalf("found %d stdlib signatures", v)
	}
}

func BenchmarkParseDump_Guess(b *testing.B) {
	b.ReportAllocs()
	data := internaltest.StaticPanicwebOutput()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := NewContext()
		if err := c.ParseDump(context.Background(), bytes.NewReader(data), ioutil.Discard); err != nil {
			b.Fatal(err)
		}
		c.GuessPaths()
		if c.Goroutines == nil {
			b.Fatal("missing context")
		}
	}
}

func BenchmarkParseDump_NoGuess(b *testing.B) {
	b.ReportAllocs()
	data := internaltest.StaticPanicwebOutput()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := NewContext()
		if err := c.ParseDump(context.Background(), bytes.NewReader(data), ioutil.Discard); err != nil {
			b.Fatal(err)
		}
		if c.Goroutines == nil {
			b.Fatal("missing context")
		}
	}
}

//

type panicwebSignatureType int

const (
	pstUnknown panicwebSignatureType = iota
	pstMain
	pstURL1handler
	pstURL2handler
	pstClient
	pstServe
	pstColorable
	pstStdlib
)

func pstCount(s []panicwebSignatureType, t panicwebSignatureType) int {
	i := 0
	for _, v := range s {
		if v == t {
			i++
		}
	}
	return i
}

func identifyPanicwebSignature(t *testing.T, b *Bucket, pwebDir string) panicwebSignatureType {
	// The first bucket (the one calling panic()) is deterministic.
	if b.First {
		if len(b.IDs) != 1 {
			t.Fatal("first bucket is not correct")
			return pstUnknown
		}
		crash := Signature{
			State: "running",
			Stack: Stack{
				Calls: []Call{
					newCallLocal("main.main", Args{}, pathJoin(pwebDir, "main.go"), 80),
				},
			},
		}
		similarSignatures(t, &crash, &b.Signature)
		return pstMain
	}

	// We should find exactly 10 sleeping routines in the URL1Handler handler
	// signature and 3 in URL2Handler.
	if s := b.Stack.Calls[0].Func.Name; s == "URL1Handler" || s == "URL2Handler" {
		if b.State != "chan receive" {
			t.Fatalf("suspicious: %#v", b)
			return pstUnknown
		}
		if b.Stack.Calls[0].ImportPath() != "github.com/maruel/panicparse/cmd/panicweb/internal" {
			t.Fatalf("suspicious: %#v", b)
			return pstUnknown
		}
		if b.Stack.Calls[0].SrcName != "internal.go" {
			t.Fatalf("suspicious: %#v", b)
			return pstUnknown
		}
		if b.CreatedBy.SrcName != "server.go" {
			t.Fatalf("suspicious: %#v", b)
			return pstUnknown
		}
		if b.CreatedBy.ImportPath() != "net/http" {
			t.Fatalf("suspicious: %#v", b)
			return pstUnknown
		}
		if b.CreatedBy.Func.Name != "(*Server).Serve" {
			t.Fatalf("suspicious: %#v", b)
			return pstUnknown
		}
		if s == "URL1Handler" {
			return pstURL1handler
		}
		return pstURL2handler
	}

	// Find the client goroutine signatures. For the client, it is likely that
	// they haven't all bucketed perfectly.
	if b.CreatedBy.ImportPath() == "github.com/maruel/panicparse/cmd/panicweb/internal" && b.CreatedBy.Func.Name == "GetAsync" {
		// TODO(maruel): More checks.
		return pstClient
	}

	// Now find the two goroutine started by main.
	if b.CreatedBy.ImportPath() == "github.com/maruel/panicparse/cmd/panicweb" && b.CreatedBy.Func.ImportPath == "main" && b.CreatedBy.Func.Name == "main" {
		if b.State == "IO wait" {
			return pstServe
		}
		if b.State == "chan receive" {
			localgopath := getGOPATHs()[0]
			// If not using Go modules, the path is different as the vendored version
			// is used instead.
			v := "@v0.1.6"
			pColorable := "pkg/mod/github.com/mattn/go-colorable" + v + "/noncolorable.go"
			pkgPrefix := ""
			if !internaltest.IsUsingModules() {
				t.Logf("Using vendored")
				pColorable = "src/github.com/maruel/panicparse/vendor/github.com/mattn/go-colorable/noncolorable.go"
				pkgPrefix = "github.com/maruel/panicparse/vendor/"
			} else {
				t.Logf("Using go module")
			}
			want := Signature{
				State:     "chan receive",
				CreatedBy: newCallLocal("main.main", Args{}, pathJoin(pwebDir, "main.go"), 73),
				Stack: Stack{
					Calls: []Call{
						newCallLocal(
							"main.(*writeHang).Write",
							Args{Values: []Arg{{}, {}, {}, {}, {}, {}, {}}},
							pathJoin(pwebDir, "main.go"),
							92),
						newCallLocal(
							pkgPrefix+"github.com/mattn/go-colorable.(*NonColorable).Write",
							Args{Values: []Arg{{}, {}, {}, {}, {}, {}, {}}},
							pathJoin(localgopath, pColorable),
							30),
					},
				},
				Locked: true,
			}
			// The arguments content is variable, so just count the number of
			// arguments and give up on the rest.
			for i := range b.Signature.Stack.Calls {
				for j := range b.Signature.Stack.Calls[i].Args.Values {
					b.Signature.Stack.Calls[i].Args.Values[j].Value = 0
					b.Signature.Stack.Calls[i].Args.Values[j].Name = ""
					b.Signature.Stack.Calls[i].Args.Values[j].IsPtr = false
				}
			}
			similarSignatures(t, &want, &b.Signature)
			return pstColorable
		}
		// That's the unix.Nanosleep() or windows.SleepEx() call.
		if b.State == "syscall" {
			created := newCallLocal(
				"main.main", Args{}, pathJoin(pwebDir, "main.go"), 63)
			zapCalls(t, &created, &b.CreatedBy)
			compareCalls(t, &created, &b.CreatedBy)
			if l := len(b.IDs); l != 1 {
				t.Fatalf("expected 1 goroutine for the signature, got %d", l)
			}
			if l := len(b.Stack.Calls); l != 4 {
				t.Fatalf("expected %d calls, got %d", 4, l)
			}
			if runtime.GOOS == "windows" {
				if s := b.Stack.Calls[0].RelSrcPath; s != "runtime/syscall_windows.go" {
					t.Fatalf("expected %q file, got %q", "runtime/syscall_windows.go", s)
				}
			} else {
				// The first item shall be an assembly file independent of the OS.
				if s := b.Stack.Calls[0].RelSrcPath; !strings.HasSuffix(s, ".s") {
					t.Fatalf("expected assembly file, got %q", s)
				}
			}
			// Process the golang.org/x/sys call specifically.
			path := "golang.org/x/sys/unix"
			fn := "Nanosleep"
			mainOS := "main_unix.go"
			if runtime.GOOS == "windows" {
				path = "golang.org/x/sys/windows"
				fn = "SleepEx"
				mainOS = "main_windows.go"
			}
			usingModules := internaltest.IsUsingModules()
			if !usingModules {
				path = "github.com/maruel/panicparse/vendor/" + path
			}
			if b.Stack.Calls[1].Func.ImportPath != path || b.Stack.Calls[1].Func.Name != fn {
				t.Fatalf("expected %q, got %#v", fn, b.Stack.Calls[1].Func)
			}
			prefix := "golang.org/x/sys@v0.0.0-"
			if !usingModules {
				// Assert that there's no version by including the trailing / and that
				// it's using the vendored version.
				prefix = "github.com/maruel/panicparse/vendor/golang.org/x/sys/"
			}
			if !strings.HasPrefix(b.Stack.Calls[1].RelSrcPath, prefix) {
				t.Fatalf("expected %q, got %q", prefix, b.Stack.Calls[1].RelSrcPath)
			}
			if usingModules {
				// Assert that it's using @v0-0-0.<date>-<commit> format.
				ver := strings.SplitN(b.Stack.Calls[1].RelSrcPath[len(prefix):], "/", 2)[0]
				re := regexp.MustCompile("^\\d{14}-[a-f0-9]{12}$")
				if !re.MatchString(ver) {
					t.Fatalf("unexpected version string %q", ver)
				}
			}
			rest := []Call{
				newCallLocal("main.sysHang", Args{}, pathJoin(pwebDir, mainOS), 12),
				newCallLocal(
					"main.main.func2",
					Args{Values: []Arg{{Value: 0xc000140720, Name: "#135", IsPtr: true}}},
					pathJoin(pwebDir, "main.go"),
					65),
			}
			got := b.Stack.Calls[2:]
			for i := range rest {
				zapCalls(t, &got[i], &rest[i])
			}
			if diff := cmp.Diff(rest, got); diff != "" {
				t.Fatalf("rest of stack mismatch (-want +got):\n%s", diff)
			}
			return pstStdlib
		}
		t.Fatalf("suspicious: %# v", b)
		return pstUnknown
	}

	// The rest should all be created with internal threads.
	if b.CreatedBy.IsStdlib {
		return pstStdlib
	}

	// On older Go version, there's often an assembly stack in asm_amd64.s.
	if b.CreatedBy.Func.Complete == "" {
		if len(b.Stack.Calls) == 1 && b.Stack.Calls[0].Func.Complete == "runtime.goexit" {
			return pstStdlib
		}
	}
	t.Fatalf("unexpected thread started by non-stdlib: %# v", b)
	return pstUnknown
}

//

// getPanicParseDir returns the path to the root directory of panicparse
// package, using "/" as path separator.
func getPanicParseDir(t *testing.T) string {
	// We assume that the working directory is the directory containing this
	// source. In Go test framework, this normally holds true. If this ever
	// becomes false, let's fix this.
	thisDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// "/" is used even on Windows in the stack trace, return in this format to
	// simply our life.
	return strings.Replace(filepath.Dir(thisDir), "\\", "/", -1)
}
