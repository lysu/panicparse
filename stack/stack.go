// Copyright 2015 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// Package stack analyzes stack dump of Go processes and simplifies it.
//
// It is mostly useful on servers will large number of identical goroutines,
// making the crash dump harder to read than strictly necesary.
package stack

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	reRoutineHeader = regexp.MustCompile("^goroutine (\\d+) \\[([^\\]]+)\\]\\:$")
	// - Sometimes the source file comes up as "<autogenerated>".
	// - Sometimes the tab is replaced with spaces.
	// - The +0x123 byte offset is not included with generated code, e.g. unnamed
	//   functions "func·006()" which is generally go func() { ... }() statements.
	reFile = regexp.MustCompile("^(?:\t| +)(\\<autogenerated\\>|.+\\.go)\\:(\\d+)(?:| \\+0x[0-9a-f]+)$")
	// Sadly, it doesn't note the goroutine number so we could cascade them per
	// parenthood.
	reCreated = regexp.MustCompile("^created by (.+)$")
	reFunc    = regexp.MustCompile("^(.+)\\((.*)\\)$")
	goroot    = runtime.GOROOT()
)

type Function struct {
	Raw string
}

// String is the fully qualified function name.
//
// Sadly Go is a bit confused when the package name doesn't match the directory
// containing the source file and will use the directory name instead of the
// real package name.
func (f Function) String() string {
	s, _ := url.QueryUnescape(f.Raw)
	return s
}

// Name is the naked function name.
func (f Function) Name() string {
	parts := strings.SplitN(filepath.Base(f.Raw), ".", 2)
	return parts[1]
}

// PkgName is the package name for this function reference.
func (f Function) PkgName() string {
	parts := strings.SplitN(filepath.Base(f.Raw), ".", 2)
	s, _ := url.QueryUnescape(parts[0])
	return s
}

// PkgDotName returns "<package>.<func>" format.
func (f Function) PkgDotName() string {
	parts := strings.SplitN(filepath.Base(f.Raw), ".", 2)
	s, _ := url.QueryUnescape(parts[0])
	if s != "" || parts[1] != "" {
		return s + "." + parts[1]
	}
	return ""
}

// IsExported returns true if the function is exported.
func (f Function) IsExported() bool {
	name := f.Name()
	parts := strings.Split(name, ".")
	r, _ := utf8.DecodeRuneInString(parts[len(parts)-1])
	if unicode.ToUpper(r) == r {
		return true
	}
	return f.PkgName() == "main" && name == "main"
}

// Call is an item in the stack trace.
type Call struct {
	SourcePath string   // Full path name of the source file
	Line       int      // Line number
	Func       Function // Fully qualified function name (encoded).
	Args       string   // Call arguments
}

// SourceName returns the file name of the source file.
func (c *Call) SourceName() string {
	return filepath.Base(c.SourcePath)
}

// SourceLine returns the source(line) format.
func (c *Call) SourceLine() string {
	return fmt.Sprintf("%s:%d", c.SourceName(), c.Line)
}

// PkgSource is one directory plus the file name of the source file.
func (c *Call) PkgSource() string {
	return filepath.Join(filepath.Base(filepath.Dir(c.SourcePath)), c.SourceName())
}

// IsStdlib returns true if it is a Go standard library function
func (c *Call) IsStdlib() bool {
	return strings.HasPrefix(c.SourcePath, goroot)
}

// IsMain returns true if it is in the main package.
func (c *Call) IsPkgMain() bool {
	return c.Func.PkgName() == "main"
}

// Goroutine represents the signature of one or multiple goroutines.
type Signature struct {
	State     string
	Stack     []Call
	CreatedBy Call // Which other goroutine which created this one.
}

func (l *Signature) Equal(r *Signature) bool {
	if l.State != r.State || len(l.Stack) != len(r.Stack) || l.CreatedBy != r.CreatedBy {
		return false
	}
	for i := range l.Stack {
		if l.Stack[i] != r.Stack[i] {
			return false
		}
	}
	return true
}

func (l *Signature) Less(r *Signature) bool {
	if l.State < r.State {
		return true
	}
	if l.State > r.State {
		return false
	}
	if len(l.Stack) < len(r.Stack) {
		return true
	}
	if len(l.Stack) > len(r.Stack) {
		return false
	}
	for x := range l.Stack {
		if l.Stack[x].Func.Raw < r.Stack[x].Func.Raw {
			return true
		}
		if l.Stack[x].Func.Raw > r.Stack[x].Func.Raw {
			return true
		}
		if l.Stack[x].PkgSource() < r.Stack[x].PkgSource() {
			return true
		}
		if l.Stack[x].PkgSource() > r.Stack[x].PkgSource() {
			return true
		}
		if l.Stack[x].Line < r.Stack[x].Line {
			return true
		}
		if l.Stack[x].Line > r.Stack[x].Line {
			return true
		}
	}
	return false
}

// Goroutine represents the state of one goroutine.
type Goroutine struct {
	Signature
	ID    int
	First bool // First is the goroutine first printed, normally the one that crashed.
}

// Bucketize returns the number of similar goroutines.
func Bucketize(goroutines []Goroutine) map[*Signature][]Goroutine {
	out := map[*Signature][]Goroutine{}
	// O(n²). Fix eventually.
	for _, routine := range goroutines {
		found := false
		for key := range out {
			if key.Equal(&routine.Signature) {
				// This effectively drops the other ID.
				out[key] = append(out[key], routine)
				found = true
				break
			}
		}
		if !found {
			key := &Signature{}
			*key = routine.Signature
			out[key] = []Goroutine{routine}
		}
	}
	return out
}

// Bucket is a stack trace signature.
type Bucket struct {
	Signature
	Routines []Goroutine
}

func (b *Bucket) First() bool {
	for _, r := range b.Routines {
		if r.First {
			return true
		}
	}
	return false
}

// Less does reverse sort.
func (b *Bucket) Less(r *Bucket) bool {
	if b.First() {
		return true
	}
	if r.First() {
		return false
	}
	if len(b.Routines) > len(r.Routines) {
		return true
	}
	if len(b.Routines) < len(r.Routines) {
		return false
	}
	return b.Signature.Less(&r.Signature)
}

// Buckets is a list of Bucket sorted by repeation count.
type Buckets []Bucket

func (b Buckets) Len() int {
	return len(b)
}

func (b Buckets) Less(i, j int) bool {
	return b[i].Less(&b[j])
}

func (b Buckets) Swap(i, j int) {
	b[j], b[i] = b[i], b[j]
}

// SortBuckets creates a list of Bucket from each goroutine stack trace count.
func SortBuckets(buckets map[*Signature][]Goroutine) Buckets {
	out := make(Buckets, 0, len(buckets))
	for signature, count := range buckets {
		out = append(out, Bucket{*signature, count})
	}
	sort.Sort(out)
	return out
}

// ParseDump processes the output from runtime.Stack().
//
// It supports piping from another command and assumes there is junk before the
// actual stack trace. The junk is streamed to out.
func ParseDump(r io.Reader, out io.Writer) ([]Goroutine, error) {
	goroutines := make([]Goroutine, 0, 16)
	var goroutine *Goroutine
	scanner := bufio.NewScanner(r)
	scanner.Split(bufio.ScanLines)
	// TODO(maruel): Use a formal state machine. Patterns follows:
	// Repeat:
	// - reFunc
	// - reFile
	// Optionally ends with:
	// - reCreated
	// - reFile
	// Between each goroutine stack dump: an empty line
	created := false
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) == 0 {
			if goroutine == nil {
				io.WriteString(out, line+"\n")
			}
			goroutine = nil
			continue
		}

		if goroutine == nil {
			if match := reRoutineHeader.FindStringSubmatch(line); match != nil {
				if id, err := strconv.Atoi(match[1]); err == nil {
					goroutines = append(goroutines, Goroutine{
						Signature: Signature{State: match[2], Stack: []Call{}},
						ID:        id,
						First:     len(goroutines) == 0,
					})
					goroutine = &goroutines[len(goroutines)-1]
					continue
				}
			}
			io.WriteString(out, line+"\n")
			continue
		}

		if match := reFile.FindStringSubmatch(line); match != nil {
			// Triggers after a reFunc or a reCreated.
			num, err := strconv.Atoi(match[2])
			if err != nil {
				return goroutines, fmt.Errorf("failed to parse int on line: \"%s\"", line)
			}
			if created {
				created = false
				goroutine.CreatedBy.SourcePath = match[1]
				goroutine.CreatedBy.Line = num
			} else {
				i := len(goroutine.Stack) - 1
				goroutine.Stack[i].SourcePath = match[1]
				goroutine.Stack[i].Line = num
			}
		} else if match := reCreated.FindStringSubmatch(line); match != nil {
			created = true
			goroutine.CreatedBy.Func.Raw = match[1]
		} else if match := reFunc.FindStringSubmatch(line); match != nil {
			goroutine.Stack = append(goroutine.Stack, Call{Func: Function{match[1]}, Args: match[2]})
		} else {
			io.WriteString(out, line+"\n")
			goroutine = nil
		}
	}
	return goroutines, scanner.Err()
}