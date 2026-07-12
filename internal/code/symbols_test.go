package code

import "testing"

func TestOutlineGo(t *testing.T) {
	src := `package sample

func Greet(name string) string {
	return name
}

type Server struct {
	Name string
}

func (s *Server) Start() error {
	return nil
}

type Handler interface {
	Handle()
}
`
	got := Outline(src, "go")
	want := map[string]struct {
		kind string
		line int
	}{
		"Greet":   {"func", 3},
		"Server":  {"struct", 7},
		"Start":   {"method", 11},
		"Handler": {"interface", 15},
	}
	if len(got) != len(want) {
		t.Fatalf("Outline() = %d symbols, want %d: %+v", len(got), len(want), got)
	}
	for _, sym := range got {
		w, ok := want[sym.Name]
		if !ok {
			t.Errorf("unexpected symbol %+v", sym)
			continue
		}
		if sym.Kind != w.kind || sym.Line != w.line {
			t.Errorf("symbol %q = %+v, want kind=%q line=%d", sym.Name, sym, w.kind, w.line)
		}
	}
}

func TestOutlinePython(t *testing.T) {
	src := `import os

def greet(name):
    return name

class Server:
    def start(self):
        pass
`
	got := Outline(src, "python")
	names := map[string]int{}
	for _, s := range got {
		names[s.Name] = s.Line
	}
	if names["greet"] != 3 {
		t.Errorf("greet at line %d, want 3", names["greet"])
	}
	if names["Server"] != 6 {
		t.Errorf("Server at line %d, want 6", names["Server"])
	}
	if names["start"] != 7 {
		t.Errorf("start at line %d, want 7", names["start"])
	}
}

func TestOutlineJavaScript(t *testing.T) {
	src := `export function main() {
  return 1
}

export const helper = (x) => x + 1

class Widget {
  render() {}
}
`
	got := Outline(src, "javascript")
	names := map[string]int{}
	for _, s := range got {
		names[s.Name] = s.Line
	}
	if names["main"] != 1 {
		t.Errorf("main at line %d, want 1", names["main"])
	}
	if names["helper"] != 5 {
		t.Errorf("helper at line %d, want 5", names["helper"])
	}
	if names["Widget"] != 7 {
		t.Errorf("Widget at line %d, want 7", names["Widget"])
	}
}

func TestOutlineRuby(t *testing.T) {
	src := `module Greeter
  class Person
    def greet
      puts "hi"
    end
  end
end
`
	got := Outline(src, "ruby")
	names := map[string]int{}
	for _, s := range got {
		names[s.Name] = s.Line
	}
	if names["Greeter"] != 1 {
		t.Errorf("Greeter at line %d, want 1", names["Greeter"])
	}
	if names["Person"] != 2 {
		t.Errorf("Person at line %d, want 2", names["Person"])
	}
	if names["greet"] != 3 {
		t.Errorf("greet at line %d, want 3", names["greet"])
	}
}

func TestOutlineRust(t *testing.T) {
	src := `pub struct Config {
    pub name: String,
}

pub fn build() -> Config {
    Config { name: String::new() }
}

pub trait Runner {
    fn run(&self);
}
`
	got := Outline(src, "rust")
	names := map[string]int{}
	for _, s := range got {
		names[s.Name] = s.Line
	}
	if names["Config"] != 1 {
		t.Errorf("Config at line %d, want 1", names["Config"])
	}
	if names["build"] != 5 {
		t.Errorf("build at line %d, want 5", names["build"])
	}
	if names["Runner"] != 9 {
		t.Errorf("Runner at line %d, want 9", names["Runner"])
	}
}

func TestOutlineCFunctionsAndControlFlowNotConfused(t *testing.T) {
	src := `int add(int a, int b) {
    if (a > b) {
        return a;
    }
    for (int i = 0; i < a; i++) {
        b++;
    }
    return b;
}
`
	got := Outline(src, "c")
	for _, s := range got {
		if s.Name == "if" || s.Name == "for" {
			t.Errorf("control-flow keyword leaked into outline: %+v", s)
		}
	}
	found := false
	for _, s := range got {
		if s.Name == "add" && s.Line == 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected add() at line 1 in outline: %+v", got)
	}
}

func TestOutlineCapsSymbolCount(t *testing.T) {
	var src string
	for i := 0; i < maxOutlineSymbols+50; i++ {
		src += "func f" + itoaTest(i) + "() {}\n"
	}
	got := Outline(src, "go")
	if len(got) > maxOutlineSymbols {
		// Outline itself does not cap — Render does, over its own call to
		// Outline — so this only pins that Outline can produce more than the
		// cap when asked, which is what exercises Render's cap.
	}
	r, err := Render([]byte(src), "text/x-go", "many.go", "")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(r.Outline) > maxOutlineSymbols {
		t.Errorf("Render outline len = %d, want <= %d", len(r.Outline), maxOutlineSymbols)
	}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
