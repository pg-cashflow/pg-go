//go:build ignore

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	var reader io.Reader = os.Stdin
	if len(os.Args) > 1 {
		f, err := os.Open(os.Args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening file %s: %v\n", os.Args[1], err)
			os.Exit(1)
		}
		defer f.Close()
		reader = f
	}

	scanner := bufio.NewScanner(reader)
	var skipped []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "--- SKIP:") {
			skipped = append(skipped, strings.TrimSpace(line))
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "Error reading input: %v\n", err)
		os.Exit(1)
	}

	if len(skipped) > 0 {
		fmt.Printf("Gate 01 Failure: %d tests were skipped:\n", len(skipped))
		for _, s := range skipped {
			fmt.Printf("  %s\n", s)
		}
		os.Exit(1)
	}

	fmt.Println("Gate 01 Success: 0 tests skipped across entire test suite.")
}
