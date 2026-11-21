package repomap

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
)

// parseGo extracts top-level declarations and identifier counts from a Go
// file. ok is false on a syntax error.
func parseGo(path string, src []byte) (syms []Symbol, idents map[string]int, ok bool) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, nil, false
	}
	// Identifier keys: "Name" for a bare identifier, "x.Name" and ".Name"
	// for a selector x.Name (x may be a package), ".Name" for other
	// selectors. Ranking uses them to tell pkg.Func from obj.Method.
	idents = map[string]int{"package " + file.Name.Name: 1}
	var visit func(n ast.Node) bool
	visit = func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.SelectorExpr:
			idents["."+e.Sel.Name]++
			if x, ok := e.X.(*ast.Ident); ok {
				idents[x.Name+"."+e.Sel.Name]++
				idents[x.Name]++
				return false
			}
			ast.Inspect(e.X, visit)
			return false
		case *ast.Ident:
			if e.Name != "_" {
				idents[e.Name]++
			}
		}
		return true
	}
	ast.Inspect(file, visit)
	cfg := printer.Config{Mode: printer.RawFormat}
	render := func(node any) string {
		var buf bytes.Buffer
		if err := cfg.Fprint(&buf, fset, node); err != nil {
			return ""
		}
		return oneLine(buf.String())
	}

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			fd := *d
			fd.Body, fd.Doc = nil, nil
			kind := "func"
			if d.Recv != nil {
				kind = "method"
			}
			syms = append(syms, Symbol{
				Kind:     kind,
				Names:    []string{d.Name.Name},
				Sig:      clip(render(&fd)),
				Line:     fset.Position(d.Pos()).Line,
				Exported: d.Name.IsExported(),
			})
		case *ast.GenDecl:
			switch d.Tok {
			case token.TYPE:
				for _, spec := range d.Specs {
					ts := spec.(*ast.TypeSpec)
					one := &ast.GenDecl{Tok: token.TYPE, Specs: []ast.Spec{stripTypeDocs(ts)}}
					syms = append(syms, Symbol{
						Kind:     "type",
						Names:    []string{ts.Name.Name},
						Sig:      clip(render(one)),
						Line:     fset.Position(ts.Pos()).Line,
						Exported: ts.Name.IsExported(),
					})
				}
			case token.CONST, token.VAR:
				// One symbol per declaration block, exported names only:
				// enough to show enums and sentinel errors without noise.
				var names []string
				for _, spec := range d.Specs {
					for _, n := range spec.(*ast.ValueSpec).Names {
						if n.IsExported() {
							names = append(names, n.Name)
						}
					}
				}
				if len(names) == 0 {
					continue
				}
				typ := ""
				if vs := d.Specs[0].(*ast.ValueSpec); vs.Type != nil {
					typ = " " + render(vs.Type)
				}
				syms = append(syms, Symbol{
					Kind:     d.Tok.String(),
					Names:    names,
					Sig:      clip(d.Tok.String() + " " + strings.Join(names, ", ") + typ),
					Line:     fset.Position(d.Pos()).Line,
					Exported: true,
				})
			}
		}
	}
	return syms, idents, true
}

// stripTypeDocs returns a copy of ts without doc/line comments on fields.
func stripTypeDocs(ts *ast.TypeSpec) *ast.TypeSpec {
	c := *ts
	c.Doc, c.Comment = nil, nil
	strip := func(fl *ast.FieldList) *ast.FieldList {
		if fl == nil {
			return nil
		}
		out := &ast.FieldList{Opening: fl.Opening, Closing: fl.Closing}
		for _, f := range fl.List {
			cf := *f
			cf.Doc, cf.Comment, cf.Tag = nil, nil, nil
			out.List = append(out.List, &cf)
		}
		return out
	}
	switch t := ts.Type.(type) {
	case *ast.StructType:
		st := *t
		st.Fields = strip(t.Fields)
		c.Type = &st
	case *ast.InterfaceType:
		it := *t
		it.Methods = strip(t.Methods)
		c.Type = &it
	}
	return &c
}

// oneLine collapses a printed multi-line declaration: "struct {\n\tA int\n}"
// becomes "struct { A int; }" → "struct { A int }".
func oneLine(s string) string {
	lines := strings.Split(s, "\n")
	var parts []string
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			parts = append(parts, l)
		}
	}
	out := strings.Join(parts, "; ")
	out = strings.ReplaceAll(out, "{; ", "{ ")
	out = strings.ReplaceAll(out, "; }", " }")
	out = strings.ReplaceAll(out, "(; ", "(")
	out = strings.ReplaceAll(out, ",; ", ", ")
	return out
}
