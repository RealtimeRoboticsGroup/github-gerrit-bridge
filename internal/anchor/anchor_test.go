package anchor

import (
	"fmt"
	"testing"
)

func TestLines(t *testing.T) {
	if got := Lines("a\nb\n"); len(got) != 2 || got[1] != "b" {
		t.Errorf("Lines = %q", got)
	}
	if got := Lines(""); got != nil {
		t.Errorf("Lines(empty) = %q", got)
	}
}

func TestPort(t *testing.T) {
	old := Lines("one\ntwo\nthree\nfour\nfive\n")
	for _, tc := range []struct {
		name string
		new  string
		want map[int]int // old line -> new line; -1 means does not port
	}{
		{"unchanged", "one\ntwo\nthree\nfour\nfive\n", map[int]int{1: 1, 3: 3, 5: 5}},
		{"insert above", "zero\none\ntwo\nthree\nfour\nfive\n", map[int]int{1: 2, 3: 4, 5: 6}},
		{"delete above", "two\nthree\nfour\nfive\n", map[int]int{1: -1, 2: 1, 5: 4}},
		{"edit the line", "one\ntwo\nTHREE\nfour\nfive\n", map[int]int{2: 2, 3: -1, 4: 4}},
		{"lines moved apart", "one\nnew a\ntwo\nnew b\nthree\nfour\nfive\n", map[int]int{1: 1, 2: 3, 3: 5}},
		{"rewritten", "x\ny\n", map[int]int{1: -1, 5: -1}},
	} {
		m := NewMap(old, Lines(tc.new))
		for from, want := range tc.want {
			got, ok := m.Line(from)
			if want == -1 {
				if ok {
					t.Errorf("%s: line %d ported to %d, want gone", tc.name, from, got)
				}
				continue
			}
			if !ok || got != want {
				t.Errorf("%s: line %d -> %d (%v), want %d", tc.name, from, got, ok, want)
			}
		}
	}
}

func TestPortRange(t *testing.T) {
	old := Lines("a\nb\nc\nd\n")
	m := NewMap(old, Lines("z\na\nb\nc\nd\n"))
	if f, l, ok := m.Range(2, 3); !ok || f != 3 || l != 4 {
		t.Errorf("Range = %d-%d %v", f, l, ok)
	}
	// A line inserted inside the range breaks it.
	m = NewMap(old, Lines("a\nb\nNEW\nc\nd\n"))
	if _, _, ok := m.Range(2, 3); ok {
		t.Error("range with an insertion inside ported")
	}
}

func TestLargeFileSmallEdit(t *testing.T) {
	var a, b []string
	for i := range 20000 {
		a = append(a, fmt.Sprintf("line %d", i))
	}
	b = append(b, a...)
	b[10000] = "changed"
	m := NewMap(a, b)
	if n, ok := m.Line(15001); !ok || n != 15001 {
		t.Errorf("line 15001 -> %d %v", n, ok)
	}
	if _, ok := m.Line(10001); ok {
		t.Error("changed line ported")
	}
}
