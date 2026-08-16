package csv

import "strings"

// StripFormulaChars removes leading spreadsheet formula markers (=, +, -, @)
// that can trigger formula injection when CSV cells are opened in Excel.
func StripFormulaChars(s string) string {
	s = strings.TrimSpace(s)
	for len(s) > 0 {
		switch s[0] {
		case '=', '+', '-', '@':
			s = strings.TrimSpace(s[1:])
		default:
			return s
		}
	}
	return s
}
