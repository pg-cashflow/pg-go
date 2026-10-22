package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

type violation struct {
	file string
	line int
	fn   string
	desc string
}

func main() {
	dirs := []string{"internal/payment", "internal/finance"}
	if len(os.Args) > 1 {
		dirs = os.Args[1:]
	}

	var violations []violation

	fset := token.NewFileSet()
	for _, dir := range dirs {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}

			node, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				return err
			}

			ast.Inspect(node, func(n ast.Node) bool {
				fnDecl, ok := n.(*ast.FuncDecl)
				if !ok || fnDecl.Body == nil {
					return true
				}

				fnName := fnDecl.Name.Name
				// Track calls in statement order
				var unlockedFetches []struct {
					varName string
					line    int
					call    string
				}

				for _, stmt := range fnDecl.Body.List {
					// Check for unlocked reads: e.g. dues.GetByID, expenses.GetByID
					ast.Inspect(stmt, func(sn ast.Node) bool {
						call, ok := sn.(*ast.CallExpr)
						if !ok {
							return true
						}
						sel, ok := call.Fun.(*ast.SelectorExpr)
						if !ok {
							return true
						}

						methodName := sel.Sel.Name
						if methodName == "GetByID" || methodName == "Get" {
							if id, ok := sel.X.(*ast.SelectorExpr); ok {
								objName := id.Sel.Name
								if objName == "dues" || objName == "expenses" {
									pos := fset.Position(call.Pos())
									unlockedFetches = append(unlockedFetches, struct {
										varName string
										line    int
										call    string
									}{
										varName: objName,
										line:    pos.Line,
										call:    fmt.Sprintf("%s.%s", objName, methodName),
									})
								}
							}
						}

						// Check if a transaction block follows: runInTx or WithinTx
						if methodName == "runInTx" || methodName == "WithinTx" {
							for _, uf := range unlockedFetches {
								pos := fset.Position(call.Pos())
								violations = append(violations, violation{
									file: path,
									line: uf.line,
									fn:   fnName,
									desc: fmt.Sprintf("unlocked fetch %s on line %d precedes transaction %s on line %d (TOCTOU race hazard)",
										uf.call, uf.line, methodName, pos.Line),
								})
							}
						}
						return true
					})
				}
				return true
			})
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "error scanning %s: %v\n", dir, err)
			os.Exit(2)
		}
	}

	if len(violations) > 0 {
		fmt.Printf("FAIL: Found %d unlocked-fetch-before-transaction pattern(s):\n", len(violations))
		for _, v := range violations {
			fmt.Printf("  %s:%d in %s(): %s\n", v.file, v.line, v.fn, v.desc)
		}
		os.Exit(1)
	}

	fmt.Println("PASS: No unlocked-fetch-before-transaction patterns found.")
}
