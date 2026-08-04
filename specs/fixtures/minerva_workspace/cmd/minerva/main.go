package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	arguments := strings.Join(os.Args[1:], " ")
	switch arguments {
	case "--version":
		fmt.Println("minerva glyph fixture")
	case "stack check":
		fmt.Println("stack check: healthy")
	case "skill list":
		fmt.Println("test-skill")
	default:
		fmt.Fprintln(os.Stderr, "unsupported fixture command:", arguments)
		os.Exit(2)
	}
}
