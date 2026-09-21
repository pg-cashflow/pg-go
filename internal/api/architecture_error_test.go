package api

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/pg-cashflow/pg-go/internal/apierr"
)

// -update rewrites internal/apierr/error-codes.json from AllCodes.
// Run with: go test ./internal/api/... -run TestErrorCodesJSONParity -update
var updateJSON = flag.Bool("update", false, "regenerate internal/apierr/error-codes.json from AllCodes")

func TestAllCodesCompleteness(t *testing.T) {
	// Parse internal/apierr/codes.go AST to extract all declared Code constants.
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, "../apierr/codes.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("failed to parse codes.go: %v", err)
	}

	declared := make(map[apierr.Code]bool)
	for _, decl := range node.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.CONST {
			continue
		}
		for _, spec := range genDecl.Specs {
			vspec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, val := range vspec.Values {
				lit, ok := val.(*ast.BasicLit)
				if ok && lit.Kind == token.STRING {
					codeVal := apierr.Code(strings.Trim(lit.Value, `"`))
					declared[codeVal] = true
				}
			}
		}
	}

	if len(declared) == 0 {
		t.Fatal("no Code constants found in codes.go")
	}

	codeRegex := regexp.MustCompile(`^[a-z]+(\.[a-zA-Z0-9]+)+$`)
	allMap := make(map[apierr.Code]bool)
	for _, c := range apierr.AllCodes {
		if allMap[c] {
			t.Errorf("duplicate code in apierr.AllCodes: %s", c)
		}
		allMap[c] = true

		if !codeRegex.MatchString(string(c)) {
			t.Errorf("code does not match domain.camelCaseReason format: %s", c)
		}

		if !declared[c] {
			t.Errorf("code %s in AllCodes is not declared as a constant in codes.go", c)
		}
	}

	for c := range declared {
		if !allMap[c] {
			t.Errorf("constant %s declared in codes.go is missing from apierr.AllCodes", c)
		}
	}
}

// extractContractCodes parses a named ERROR_CODES_* section from CONTRACT.md.
func extractContractCodes(content, startMarker, endMarker string) (map[apierr.Code]bool, error) {
	startIdx := strings.Index(content, startMarker)
	if startIdx == -1 {
		return nil, fmt.Errorf("CONTRACT.md missing marker: %s", startMarker)
	}
	endIdx := strings.Index(content[startIdx:], endMarker)
	if endIdx == -1 {
		return nil, fmt.Errorf("CONTRACT.md missing marker: %s", endMarker)
	}
	section := content[startIdx+len(startMarker) : startIdx+endIdx]
	codeRegex := regexp.MustCompile("`([a-z]+\\.[a-zA-Z0-9]+)`")
	codes := make(map[apierr.Code]bool)
	for _, m := range codeRegex.FindAllStringSubmatch(section, -1) {
		codes[apierr.Code(m[1])] = true
	}
	return codes, nil
}

func TestContractMarkdownTableParity(t *testing.T) {
	contractBytes, err := os.ReadFile("../../CONTRACT.md")
	if err != nil {
		t.Fatalf("failed to read CONTRACT.md: %v", err)
	}
	content := strings.ReplaceAll(string(contractBytes), "\r\n", "\n")

	contractCodes, err := extractContractCodes(content,
		"<!-- ERROR_CODES_START -->", "<!-- ERROR_CODES_END -->")
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range apierr.AllCodes {
		if !contractCodes[c] {
			t.Errorf("code %s in apierr.AllCodes is missing from CONTRACT.md primary table", c)
		}
	}

	for c := range contractCodes {
		found := false
		for _, ac := range apierr.AllCodes {
			if ac == c {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("code %s documented in CONTRACT.md primary table is missing from apierr.AllCodes", c)
		}
	}
}

// TestBothContractBlocksMatch asserts the primary ERROR_CODES table and the
// ERROR_CODES_DETAIL table contain exactly the same set of codes.
// This prevents the two tables from silently drifting.
func TestBothContractBlocksMatch(t *testing.T) {
	contractBytes, err := os.ReadFile("../../CONTRACT.md")
	if err != nil {
		t.Fatalf("failed to read CONTRACT.md: %v", err)
	}
	content := strings.ReplaceAll(string(contractBytes), "\r\n", "\n")

	primary, err := extractContractCodes(content,
		"<!-- ERROR_CODES_START -->", "<!-- ERROR_CODES_END -->")
	if err != nil {
		t.Fatal(err)
	}
	detail, err := extractContractCodes(content,
		"<!-- ERROR_CODES_DETAIL_START -->", "<!-- ERROR_CODES_DETAIL_END -->")
	if err != nil {
		t.Fatal(err)
	}

	for c := range primary {
		if !detail[c] {
			t.Errorf("code %s in primary ERROR_CODES table missing from ERROR_CODES_DETAIL table", c)
		}
	}
	for c := range detail {
		if !primary[c] {
			t.Errorf("code %s in ERROR_CODES_DETAIL table missing from primary ERROR_CODES table", c)
		}
	}
}

// jsonPath is the canonical source of truth for error codes consumed by pg-react.
// It lives inside pg-go so it can be regenerated with -update and committed.
const jsonPath = "../apierr/error-codes.json"

func TestErrorCodesJSONParity(t *testing.T) {
	if *updateJSON {
		regenerateErrorCodesJSON(t)
		return
	}

	data, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("failed to read %s: %v — run with -update to regenerate", jsonPath, err)
	}

	var jsonCodes []apierr.Code
	if err := json.Unmarshal(data, &jsonCodes); err != nil {
		t.Fatalf("failed to unmarshal %s: %v", jsonPath, err)
	}

	if len(jsonCodes) != len(apierr.AllCodes) {
		t.Errorf("length mismatch: %s has %d, apierr.AllCodes has %d — run with -update",
			jsonPath, len(jsonCodes), len(apierr.AllCodes))
	}

	codeMap := make(map[apierr.Code]bool)
	for _, c := range jsonCodes {
		codeMap[c] = true
	}

	for _, c := range apierr.AllCodes {
		if !codeMap[c] {
			t.Errorf("code %s in AllCodes missing from %s — run with -update", c, jsonPath)
		}
	}
}

func regenerateErrorCodesJSON(t *testing.T) {
	t.Helper()
	codes := make([]string, len(apierr.AllCodes))
	for i, c := range apierr.AllCodes {
		codes[i] = string(c)
	}
	data, err := json.MarshalIndent(codes, "", "  ")
	if err != nil {
		t.Fatalf("failed to marshal AllCodes: %v", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(jsonPath, data, 0o644); err != nil {
		t.Fatalf("failed to write %s: %v", jsonPath, err)
	}
	t.Logf("regenerated %s with %d codes", jsonPath, len(apierr.AllCodes))
}

// TestAllCodesHaveEmitSites verifies that every code in AllCodes is referenced
// in at least one non-test, non-codes.go Go source file. This prevents dead
// codes that have no emit site from accumulating in the registry.
func TestAllCodesHaveEmitSites(t *testing.T) {
	// Parse internal/apierr/codes.go AST to map Code value to constant identifier name.
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, "../apierr/codes.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("failed to parse codes.go: %v", err)
	}

	codeToConst := make(map[apierr.Code]string)
	for _, decl := range node.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.CONST {
			continue
		}
		for _, spec := range genDecl.Specs {
			vspec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, val := range vspec.Values {
				lit, ok := val.(*ast.BasicLit)
				if ok && lit.Kind == token.STRING {
					codeVal := apierr.Code(strings.Trim(lit.Value, `"`))
					if i < len(vspec.Names) {
						codeToConst[codeVal] = vspec.Names[i].Name
					}
				}
			}
		}
	}

	// Scan all non-test Go source files in internal/api, internal/auth, internal/payment.
	scanDirs := []string{"../api", "../auth", "../payment"}
	allSource := ""
	for _, dir := range scanDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("failed to read dir %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			if strings.HasSuffix(e.Name(), "_test.go") || e.Name() == "codes.go" {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatalf("failed to read %s/%s: %v", dir, e.Name(), err)
			}
			allSource += string(data)
		}
	}

	for _, c := range apierr.AllCodes {
		constName := codeToConst[c]
		codeStr := string(c)
		hasConst := constName != "" && strings.Contains(allSource, constName)
		hasStr := strings.Contains(allSource, codeStr)
		if !hasConst && !hasStr {
			t.Errorf("code %s (const %s) has no emit site in scanned source files", c, constName)
		}
	}
}

// TestArchitectureAST4xx5xx verifies that all 4xx/5xx responses in migrated packages
// use the apierr envelope rather than ad-hoc maps, and that no new unmigrated
// 4xx/5xx call sites can be added without failing CI.
func TestArchitectureAST4xx5xx(t *testing.T) {
	// Permanent exceptions (non-JSON responses — intentional):
	//   PaymentPage:     serves HTML payment page via template.Execute
	//   DueQR:           serves raw PNG bytes via c.Data
	//   CashfreeWebhook: processes raw HMAC gateway callback
	permanentExceptions := map[string]bool{
		"PaymentPage":     true,
		"DueQR":           true,
		"CashfreeWebhook": true,
	}

	// Migrated files: 100% of 4xx/5xx responses MUST use the apierr envelope.
	// Uncoded-emit allowlist (files with remaining raw gin.H 4xx/5xx — see burn-down plan below):
	//
	//   handlers_owner.go       — "invalid body", "not found", "list failed", "rent_amount must be positive",
	//                             "no id photo", "body too large", "file too large (max 5MB)"
	//   handlers_pay.go         — "not found", "already recorded", "upi_txn_id required", "body too large",
	//                             "image must be jpeg, png, or webp", "already recorded"
	//   handlers_tenant.go      — "not found"
	//   handlers_join.go        — "not found", "invalid invite code", "complete your profile to continue",
	//                             "invalid multipart body", "invalid body", "property", "list failed"
	//   handlers_gamification.go  (committed, clean) — "invalid reward id", "invalid …"
	//   handlers_notifications.go (committed, clean) — "invalid notification id"
	//   handlers_finance.go     (new, untracked) — finance error sites
	//   handlers_search.go      (new, untracked) — search error sites
	//
	// Burn-down: migrate one domain at a time. When a file is fully migrated,
	// add it to migratedFiles below and remove its entry from this comment.
	migratedFiles := map[string]bool{
		"middleware.go":           true,
		"apierr.go":               true,
		"handlers_preferences.go": true,
		"handlers_public.go":      true,
	}

	fset := token.NewFileSet()
	packagesToScan := []string{"../auth", "../api"}

	for _, pkgDir := range packagesToScan {
		pkgs, err := parser.ParseDir(fset, pkgDir, func(fi os.FileInfo) bool {
			return strings.HasSuffix(fi.Name(), ".go") && !strings.HasSuffix(fi.Name(), "_test.go")
		}, parser.ParseComments)
		if err != nil {
			t.Fatalf("failed to parse directory %s: %v", pkgDir, err)
		}

		for _, pkg := range pkgs {
			for filePath, fileNode := range pkg.Files {
				fileName := filepath.Base(filePath)
				if strings.Contains(filePath, "apierr") {
					continue
				}

				ast.Inspect(fileNode, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}

					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}

					methodName := sel.Sel.Name
					if methodName != "JSON" && methodName != "AbortWithStatusJSON" && methodName != "String" {
						return true
					}

					if len(call.Args) == 0 {
						return true
					}

					// Check if first arg is an HTTP 4xx/5xx status
					statusArg := call.Args[0]
					is4xx5xx := false

					switch s := statusArg.(type) {
					case *ast.BasicLit:
						if s.Kind == token.INT {
							var val int
							_, _ = fmt.Sscanf(s.Value, "%d", &val)
							if val >= 400 {
								is4xx5xx = true
							}
						}
					case *ast.SelectorExpr:
						// http.StatusBadRequest, etc.
						if pkgIdent, ok := s.X.(*ast.Ident); ok && pkgIdent.Name == "http" {
							name := s.Sel.Name
							if strings.HasPrefix(name, "Status") {
								// All 4xx and 5xx names in net/http
								if strings.Contains(name, "Bad") || strings.Contains(name, "Unauthorized") ||
									strings.Contains(name, "Forbidden") || strings.Contains(name, "NotFound") ||
									strings.Contains(name, "Conflict") || strings.Contains(name, "Gone") ||
									strings.Contains(name, "InternalServer") || strings.Contains(name, "ServiceUnavailable") ||
									strings.Contains(name, "TooManyRequests") || strings.Contains(name, "RequestEntityTooLarge") {
									is4xx5xx = true
								}
							}
						}
					}

					if !is4xx5xx {
						return true
					}

					// Check if this call is within a permanent exception function
					pos := fset.Position(call.Pos())
					for fnName := range permanentExceptions {
						if strings.Contains(pos.String(), fnName) {
							return true
						}
					}

					// If in a migrated file, verify it uses apierr envelope
					if migratedFiles[fileName] {
						if len(call.Args) >= 2 {
							bodyArg := call.Args[1]
							// Check if body is raw gin.H or composite literal not using apierr
							if comp, ok := bodyArg.(*ast.CompositeLit); ok {
								if selType, ok := comp.Type.(*ast.SelectorExpr); ok {
									if selType.Sel.Name == "H" {
										t.Errorf("%s:%d: migrated file %s emits raw gin.H for 4xx/5xx — must use apierr envelope helper",
											fileName, pos.Line, fileName)
									}
								}
							}
						}
					}

					return true
				})
			}
		}
	}
}
