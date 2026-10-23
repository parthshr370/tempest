package hashline

import (
	"strings"
	"testing"
)

// blockCase expects line to resolve to end, or to fail with a problem
// containing problem.
type blockCase struct {
	line    int
	end     int
	problem string
}

func checkBlocks(t *testing.T, path, text string, cases []blockCase) {
	t.Helper()
	lines := Lines(text)
	res, ok := newResolver(path, text, lines)
	if !ok {
		t.Fatalf("%s: no resolver", path)
	}
	for _, c := range cases {
		end, problem := res(c.line)
		if c.problem != "" {
			if problem == "" || !strings.Contains(problem, c.problem) {
				t.Errorf("%s line %d: got end %d problem %q, want problem %q", path, c.line, end, problem, c.problem)
			}
			continue
		}
		if problem != "" || end != c.end {
			t.Errorf("%s line %d (%q): got end %d problem %q, want end %d", path, c.line, lines[c.line-1], end, problem, c.end)
		}
	}
}

func TestGoBlocks(t *testing.T) {
	src := `package p

// Doc for F.
// Second doc line.
func F(a int,
	b int) int {
	if a > b {
		return a
	} else {
		return b
	}
}

// standalone comment

var table = map[string]int{
	"a": 1,
}

func G() { F(1, 2) }

type T struct {
	// Name doc.
	Name string
	Tags []string
}
`
	checkBlocks(t, "p.go", src, []blockCase{
		{line: 3, end: 12},                          // doc comment sweeps its function
		{line: 5, end: 12},                          // the func line starts the decl, doc excluded
		{line: 7, end: 11},                          // if/else statement
		{line: 8, end: 8},                           // single statement resolves to one line
		{line: 16, end: 18},                         // composite literal declaration
		{line: 20, end: 20},                         // one-line func
		{line: 22, end: 26},                         // struct type
		{line: 23, end: 24},                         // field doc sweeps the field
		{line: 4, problem: "does not begin"},        // inside a doc comment
		{line: 9, problem: "closing delimiter"},     // } else {
		{line: 12, problem: "closing delimiter"},    // }
		{line: 14, problem: "standalone comment"},   // comment that documents nothing
		{line: 2, problem: "blank"},                 // blank line
		{line: 1, problem: "does not begin a Go d"}, // package clause is not a block
	})
	checkBlocks(t, "bad.go", "package p\nfunc {\n", []blockCase{{line: 2, problem: "does not parse"}})
}

func TestMarkdownBlocks(t *testing.T) {
	src := "# Title\n\nintro\n\n## A\n\ntext\n\n### A.1\n\nmore\n\n```sh\n# not a heading\n```\n\n## B\n\nlast\n"
	checkBlocks(t, "doc.md", src, []blockCase{
		{line: 1, end: 19},  // whole document under the top heading
		{line: 5, end: 16},  // section A through its deeper heading and fence, up to ## B
		{line: 9, end: 16},  // ### A.1 to the next same-or-higher heading
		{line: 13, end: 15}, // code fence
		{line: 17, end: 19}, // last section runs to the end
		{line: 14, problem: "inside a fenced code block"},
		{line: 3, problem: "not a Markdown heading"},
	})
}

func TestBraceBlocks(t *testing.T) {
	js := "/** Docs. */\nexport function f(a) {\n  const s = \"}\" + '{' + `${a}}`;\n  // }\n  return s;\n}\n\nif (x) {\n  y();\n}\nelse {\n  z();\n}\nconst o = {\n  a: [1, 2],\n};\nfoo()\n  .bar(() => {\n  });\n"
	checkBlocks(t, "f.ts", js, []blockCase{
		{line: 1, end: 6},  // doc comment sweeps the function
		{line: 2, end: 6},  // braces inside strings, templates and comments are ignored
		{line: 8, end: 13}, // else on its own line continues the if
		{line: 14, end: 16},
		{line: 17, end: 19}, // chained call
		{line: 5, end: 5},   // single statement
		{line: 4, problem: "standalone comment"},
		{line: 6, problem: "closing delimiter"},
	})
	rust := "#[derive(Debug)]\npub struct S<'a> {\n    name: &'a str,\n    c: char,\n}\n\nimpl<'a> S<'a> {\n    fn f(&self) -> char {\n        '}'\n    }\n}\n"
	checkBlocks(t, "s.rs", rust, []blockCase{
		{line: 1, end: 5}, // attribute sweeps the struct; lifetimes are not char literals
		{line: 2, end: 5},
		{line: 7, end: 11}, // char literal '}' is skipped
		{line: 8, end: 10},
	})
	java := "@Override\n@SuppressWarnings({\"x\"})\npublic void run()\n{\n    go();\n}\n"
	checkBlocks(t, "A.java", java, []blockCase{
		{line: 1, end: 6}, // stacked annotations, Allman brace
		{line: 3, end: 6},
	})
	cs := "[Fact]\npublic void Test() {\n    var s = @\"C:\\{\";\n}\n"
	checkBlocks(t, "T.cs", cs, []blockCase{{line: 1, end: 4}, {line: 2, end: 4}})
	json := "{\n  \"a\": {\n    \"b\": \"}\"\n  },\n  \"c\": [\n    1\n  ]\n}\n"
	checkBlocks(t, "x.json", json, []blockCase{{line: 1, end: 8}, {line: 2, end: 4}, {line: 5, end: 7}})
	checkBlocks(t, "y.ts", "function f() {\n  if (a) {\n", []blockCase{{line: 1, problem: "never close"}})
	checkBlocks(t, "z.ts", "function f() {\n  a();\n}\n", []blockCase{{line: 2, end: 2}})
}

func TestPythonBlocks(t *testing.T) {
	src := "@cache\n@other(\n    1)\ndef load(key):\n    data = {\n        'k': 1,\n    }\n\n    return data\n\nif a:\n    x()\nelif b:\n    y()\nelse:\n    z()\nprint(1)\n# comment\n"
	checkBlocks(t, "m.py", src, []blockCase{
		{line: 1, end: 9}, // decorators sweep the definition; trailing blank excluded
		{line: 4, end: 9},
		{line: 5, end: 7},   // bracketed continuation lines belong to the statement
		{line: 11, end: 16}, // if/elif/else
		{line: 17, end: 17},
		{line: 6, problem: "continues the statement"},
		{line: 18, problem: "standalone comment"},
	})
}

func TestYAMLBlocks(t *testing.T) {
	src := "spec:\n  containers:\n  - name: a\n    image: x\n  - name: b\n  replicas: 2\nother: 1\n# c\n"
	checkBlocks(t, "d.yaml", src, []blockCase{
		{line: 1, end: 6},
		{line: 2, end: 5}, // key with a same-indent sequence
		{line: 3, end: 4}, // one sequence item
		{line: 6, end: 6},
		{line: 8, problem: "standalone comment"},
	})
}

func TestBlockOpsThroughPatch(t *testing.T) {
	src := "package p\n\n// F doc.\nfunc F() int {\n\treturn 1\n}\n\nfunc G() int {\n\treturn 2\n}\n"
	files := memFS{"p.go": src}
	store := NewStore()
	tag := readAll(store, files, "p.go")
	previews, err := apply(t, store, files, "[p.go#"+tag+"]\nPUT 8*:\n+func G() int {\n+\treturn 3\n+}\nCUT 3* @f\nPUT >8* @f\n")
	if err != nil {
		t.Fatal(err)
	}
	want := "package p\n\n\nfunc G() int {\n\treturn 3\n}\n// F doc.\nfunc F() int {\n\treturn 1\n}\n"
	if files["p.go"] != want {
		t.Fatalf("got %q", files["p.go"])
	}
	if previews["p.go"] == "" {
		t.Fatal("no preview")
	}
	// Block errors name a fallback and the enclosing construct.
	tag = readAll(store, files, "p.go")
	_, err = apply(t, store, files, "[p.go#"+tag+"]\nPUT 5*:\n+x\n")
	if err == nil || !strings.Contains(err.Error(), "single line") || !strings.Contains(err.Error(), "PUT 4*:") {
		t.Fatalf("err = %v", err)
	}
	_, err = apply(t, store, files, "[p.go#"+tag+"]\nPUT 6*:\n+x\n")
	if err == nil || !strings.Contains(err.Error(), "closing delimiter") || !strings.Contains(err.Error(), "PUT 6.=M:") {
		t.Fatalf("err = %v", err)
	}
}
