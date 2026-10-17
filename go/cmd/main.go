package main

import (
	"os"

	actions "github.com/rebis-org/actions/internal"
)

func main() {
	os.Exit(actions.Execute(os.Args[1:]))
}
