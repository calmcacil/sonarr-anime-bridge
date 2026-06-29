package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestSlogCallsIncludeTypeField(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve test file path")
	}
	cmdServerDir := filepath.Dir(thisFile)
	moduleRoot := filepath.Clean(filepath.Join(cmdServerDir, "..", ".."))
	roots := []string{cmdServerDir, filepath.Join(moduleRoot, "internal")}
	var files []string

	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == ".git" || d.Name() == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			files = append(files, path)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	if len(files) == 0 {
		t.Fatal("no source files to inspect for slog calls")
	}

	fset := token.NewFileSet()
	for _, filename := range files {
		file, err := parser.ParseFile(fset, filename, nil, parser.AllErrors)
		if err != nil {
			// Non-parseable files are possible in fixture/test scaffolding;
			// skip with test-visible diagnostics.
			t.Logf("parse %s: %v", filename, err)
			continue
		}

		for _, decl := range file.Decls {
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "slog" {
					return true
				}
				switch sel.Sel.Name {
				case "Debug", "Info", "Warn", "Error":
					if !slogArgsHaveType(call.Args) {
						pos := fset.Position(call.Lparen)
						t.Errorf("%s: slog.%s call missing `type` key", pos.String(), sel.Sel.Name)
					}
				}
				return true
			})
		}
	}
}

func slogArgsHaveType(args []ast.Expr) bool {
	if len(args) < 2 {
		return false
	}
	for i := 1; i+1 < len(args); i += 2 {
		key, ok := args[i].(*ast.BasicLit)
		if !ok {
			continue
		}
		s, err := strconv.Unquote(key.Value)
		if err != nil {
			continue
		}
		if s == "type" {
			return true
		}
	}
	return false
}
