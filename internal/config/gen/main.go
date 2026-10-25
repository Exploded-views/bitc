// Command gen prints the generated configuration reference.
package main

import (
	"fmt"

	"github.com/Exploded-views/bitc/internal/config"
)

func main() { fmt.Print(config.GenerateReference()) }
