package repl

import (
	"reflect"
	"sort"
	"testing"
)

func TestFilterAndOrder_PrefixMatch(t *testing.T) {
	out := filterAndOrder([]string{"send", "recv", "close", "len"}, "")
	got := runesToStrs(out)

	want := []string{"close", "len", "recv", "send"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("empty partial: got %v, want %v", got, want)
	}
}

func TestFilterAndOrder_PartialMatch(t *testing.T) {
	out := filterAndOrder([]string{"send", "send_all", "recv", "close"}, "se")
	got := runesToStrs(out)

	want := []string{"nd", "nd_all"}
	sort.Strings(want)

	if !reflect.DeepEqual(got, want) {
		t.Errorf("partial 'se': got %v, want %v", got, want)
	}
}

func TestTypeNameOfGlobal(t *testing.T) {
	s := &session{
		prevGlobals: map[string]string{
			"ch":     "var ch sync::Channel[string]",
			"v":      "var v Vec3",
			"p":      "var p *Node",
			"plain":  "var plain i64",
			"nested": "var nested Box[List[i64]]",
		},
	}

	cases := map[string]string{
		"ch":     "Channel",
		"v":      "Vec3",
		"p":      "Node",
		"plain":  "i64",
		"nested": "Box",
		"nope":   "",
	}

	for in, want := range cases {
		got := s.typeNameOfGlobal(in)
		if got != want {
			t.Errorf("typeNameOfGlobal(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScanStructMembers(t *testing.T) {
	src := `
struct Vec3(add[Vec3, Vec3]) =
  x f64
  y f64
  z f64

  fn ::add(this Vec3, other Vec3) Vec3 = return this
  static fn new() Vec3 = return Vec3{x: 0.0, y: 0.0, z: 0.0}
`
	got := scanStructMembers(src, "Vec3")

	want := []string{"x", "y", "z", "add"} // static `new` excluded
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scanStructMembers: got %v, want %v", got, want)
	}
}

func TestFormattedFnName(t *testing.T) {
	cases := map[string]string{
		"fn sync::send(x i64)":            "send",
		"fn sync::recv() string":          "recv",
		"fn assert::equals(a i64, b i64)": "equals",
		"fn   pkg::name  ":                "name",
	}

	for in, want := range cases {
		got := formattedFnName(in)
		if got != want {
			t.Errorf("formattedFnName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStripGenericsTail(t *testing.T) {
	cases := map[string]string{
		"Foo":              "Foo",
		"Foo[T]":           "Foo",
		"Foo[Bar[Baz]]":    "Foo",
		"sync::Channel[T]": "sync::Channel",
		"":                 "",
		"Foo[":             "Foo[", // not ending in ]
		"Foo[]":            "Foo",
	}

	for in, want := range cases {
		got := stripGenericsTail(in)
		if got != want {
			t.Errorf("stripGenericsTail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitLastScope(t *testing.T) {
	cases := []struct {
		in       string
		wantPkg  string
		wantName string
	}{
		{"sync", "sync", ""},
		{"sync::Channel", "sync", "Channel"},
		{"a::b::c", "a::b", "c"},
		{"", "", ""},
	}

	for _, tc := range cases {
		gotPkg, gotName := splitLastScope(tc.in)
		if gotPkg != tc.wantPkg || gotName != tc.wantName {
			t.Errorf("splitLastScope(%q) = (%q, %q), want (%q, %q)",
				tc.in, gotPkg, gotName, tc.wantPkg, tc.wantName)
		}
	}
}

func TestLastTopLevelDot(t *testing.T) {
	cases := map[string]int{
		"":                      -1,
		"foo":                   -1,
		"a.b":                   1,
		"a.b.c":                 3,
		"Foo[Bar].method":       8,
		"Foo[Bar.baz]":          -1, // dot is inside brackets
		"sync::Channel[string]": -1,
		"a.b[c.d]":              1,
	}

	for in, want := range cases {
		got := lastTopLevelDot(in)
		if got != want {
			t.Errorf("lastTopLevelDot(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestWalkReceiverBack(t *testing.T) {
	cases := []struct {
		src  string
		end  int
		want int
	}{
		{"obj", 3, 0},
		{"sync::Channel", 13, 0},
		{"sync::Channel[string]", 21, 0},
		{"  sync::Foo[Bar[Baz]]", 21, 2},
		{"obj.field.x", 11, 0},
		{"x = obj.field", 13, 4},
		{"foo[Bar[Baz]]", 13, 0},
		{"trail (obj", 10, 7},
	}

	for _, tc := range cases {
		got := walkReceiverBack(tc.src, tc.end)
		if got != tc.want {
			t.Errorf("walkReceiverBack(%q, %d) = %d, want %d",
				tc.src, tc.end, got, tc.want)
		}
	}
}

func runesToStrs(rs [][]rune) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = string(r)
	}

	return out
}

func TestCompleter_Do_StaticMethodsOnStruct(t *testing.T) {
	// `Name::partial` should also surface a session struct's static methods.
	src := `
struct Vec3 =
  x f64
  static fn new() Vec3 = return Vec3{x: 0.0}
  static fn unit() Vec3 = return Vec3{x: 1.0}
  fn len(this Vec3) f64 = return 0.0
`
	s := &session{
		declMap:   map[string]string{"Vec3": src},
		declOrder: []string{"Vec3"},
	}

	c := &completer{s: s}
	line := []rune("Vec3::")

	cands, n := c.Do(line, len(line))
	if n != 0 {
		t.Errorf("prefix length: got %d, want 0", n)
	}

	got := runesToStrs(cands)

	want := []string{"new", "unit"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestCompleter_Do_StaticMethodsViaDot(t *testing.T) {
	// `Vec3.partial` should also surface static methods (matches `Type.method()`).
	src := `
struct Vec3 =
  x f64
  static fn new() Vec3 = return Vec3{x: 0.0}
  static fn unit() Vec3 = return Vec3{x: 1.0}
`
	s := &session{
		declMap:   map[string]string{"Vec3": src},
		declOrder: []string{"Vec3"},
	}

	c := &completer{s: s}
	line := []rune("Vec3.un")

	cands, n := c.Do(line, len(line))
	if n != 2 {
		t.Errorf("prefix length: got %d, want 2", n)
	}

	got := runesToStrs(cands)

	want := []string{"it"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestCompleter_Do_FieldFromGlobal(t *testing.T) {
	src := `
struct Box =
  v i64
  fn open(this Box) i64 = return this.v
  fn close(this Box) i64 = return 0
  fn close_silent(this Box) i64 = return 0
`
	s := &session{
		declMap:     map[string]string{"Box": src},
		declOrder:   []string{"Box"},
		prevGlobals: map[string]string{"b": "var b Box"},
	}

	c := &completer{s: s}
	line := []rune("b.cl")

	cands, n := c.Do(line, len(line))
	if n != 2 { // "cl"
		t.Errorf("prefix length: got %d, want 2", n)
	}

	got := runesToStrs(cands)

	want := []string{"ose", "ose_silent"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestCompleter_Do_FieldChain(t *testing.T) {
	// `obj.field.partial` -> methods of field's type.
	src := `
struct Inner =
  v i64
  fn touch(this Inner) i64 = return 0
  fn tap(this Inner) i64 = return 0

struct Outer =
  inner Inner
`
	s := &session{
		declMap:     map[string]string{"Inner": src, "Outer": src},
		declOrder:   []string{"Inner", "Outer"},
		prevGlobals: map[string]string{"o": "var o Outer"},
	}

	c := &completer{s: s}
	line := []rune("o.inner.t")

	cands, n := c.Do(line, len(line))
	if n != 1 {
		t.Errorf("prefix length: got %d, want 1", n)
	}

	got := runesToStrs(cands)

	want := []string{"ap", "ouch"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestCanonicalTypeName(t *testing.T) {
	cases := map[string]string{
		"Vec3":                  "Vec3",
		"sync::Channel":         "Channel",
		"sync::Channel[string]": "Channel",
		"*Node":                 "Node",
		"**Node":                "Node",
		"Box[List[i64]]":        "Box",
		"sync::Foo[Bar[Baz]]":   "Foo",
	}

	for in, want := range cases {
		got := canonicalTypeName(in)
		if got != want {
			t.Errorf("canonicalTypeName(%q) = %q, want %q", in, got, want)
		}
	}
}
