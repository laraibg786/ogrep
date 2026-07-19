package walk

import "testing"

func TestParsePatternSkipsBlankAndComments(t *testing.T) {
	cases := []string{"", "   ", "#comment"}
	for _, c := range cases {
		if _, ok := parsePattern(c); ok {
			t.Errorf("parsePattern(%q) should be skipped", c)
		}
	}
}

func TestPatternSetBasicGlob(t *testing.T) {
	ps := ParsePatternLines([]string{"*.log"})
	if !ps.Match("debug.log", false) {
		t.Error("expected debug.log to be ignored")
	}
	if ps.Match("debug.log.txt", false) {
		t.Error("did not expect debug.log.txt to be ignored")
	}
	if !ps.Match("sub/dir/debug.log", false) {
		t.Error("expected nested debug.log to be ignored (unanchored pattern matches at any depth)")
	}
}

func TestPatternSetAnchoredPattern(t *testing.T) {
	// A pattern containing a "/" (other than a trailing one) is
	// anchored to the ignore file's directory: "build/output.txt" only
	// matches that exact relative path, not "sub/build/output.txt".
	ps := ParsePatternLines([]string{"build/output.txt"})
	if !ps.Match("build/output.txt", false) {
		t.Error("expected anchored pattern to match at the root")
	}
	if ps.Match("sub/build/output.txt", false) {
		t.Error("anchored pattern should not match when nested deeper")
	}
}

func TestPatternSetLeadingSlashAnchors(t *testing.T) {
	ps := ParsePatternLines([]string{"/only-at-root.txt"})
	if !ps.Match("only-at-root.txt", false) {
		t.Error("expected leading-slash pattern to match at the root")
	}
	if ps.Match("nested/only-at-root.txt", false) {
		t.Error("leading-slash pattern should not match nested path")
	}
}

func TestPatternSetDirectoryOnly(t *testing.T) {
	ps := ParsePatternLines([]string{"build/"})
	if !ps.Match("build", true) {
		t.Error("expected directory-only pattern to match a directory named build")
	}
	if ps.Match("build", false) {
		t.Error("directory-only pattern should not match a regular file named build")
	}
}

func TestPatternSetDoubleStarMiddle(t *testing.T) {
	ps := ParsePatternLines([]string{"a/**/b"})
	tests := map[string]bool{
		"a/b":     true,
		"a/x/b":   true,
		"a/x/y/b": true,
		"a/x/y/c": false,
		"x/a/b":   false, // anchored: must start at "a"
	}
	for path, want := range tests {
		if got := ps.Match(path, false); got != want {
			t.Errorf("Match(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestPatternSetDoubleStarPrefix(t *testing.T) {
	ps := ParsePatternLines([]string{"**/foo.txt"})
	if !ps.Match("foo.txt", false) {
		t.Error("expected leading **/ to match at root too")
	}
	if !ps.Match("a/b/c/foo.txt", false) {
		t.Error("expected leading **/ to match at any depth")
	}
}

func TestPatternSetDoubleStarSuffix(t *testing.T) {
	ps := ParsePatternLines([]string{"logs/**"})
	if !ps.Match("logs/a.txt", false) {
		t.Error("expected trailing /** to match everything under logs/")
	}
	if !ps.Match("logs/sub/a.txt", false) {
		t.Error("expected trailing /** to match nested paths under logs/")
	}
	if ps.Match("other/a.txt", false) {
		t.Error("did not expect unrelated path to match")
	}
}

func TestPatternSetNegation(t *testing.T) {
	// Standard gitignore idiom: ignore everything in a directory except
	// one file, using negation. Order matters — later patterns win.
	ps := ParsePatternLines([]string{
		"*.log",
		"!important.log",
	})
	if !ps.Match("debug.log", false) {
		t.Error("expected debug.log to be ignored")
	}
	if ps.Match("important.log", false) {
		t.Error("expected important.log to be un-ignored by negation")
	}
}

func TestPatternSetNegationOrderMatters(t *testing.T) {
	// If the broad ignore comes *after* the negation, the negation has
	// no effect (matches git's last-match-wins semantics).
	ps := ParsePatternLines([]string{
		"!important.log",
		"*.log",
	})
	if !ps.Match("important.log", false) {
		t.Error("expected important.log to be ignored since the broader pattern comes last")
	}
}

func TestPatternSetQuestionMarkAndCharClass(t *testing.T) {
	ps := ParsePatternLines([]string{"file?.txt", "[abc].log"})
	if !ps.Match("file1.txt", false) {
		t.Error("expected file1.txt to match file?.txt")
	}
	if ps.Match("file12.txt", false) {
		t.Error("did not expect file12.txt to match file?.txt")
	}
	if !ps.Match("a.log", false) {
		t.Error("expected a.log to match [abc].log")
	}
	if ps.Match("d.log", false) {
		t.Error("did not expect d.log to match [abc].log")
	}
}

func TestPatternSetEmpty(t *testing.T) {
	var ps PatternSet
	if !ps.Empty() {
		t.Error("zero-value PatternSet should be Empty")
	}
	if ps.Match("anything", false) {
		t.Error("empty PatternSet should never match")
	}
}

