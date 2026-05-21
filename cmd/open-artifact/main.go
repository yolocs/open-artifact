// Command open-artifact is the open-artifact server: a stateless, multi-format
// artifact registry backed by a gocloud.dev/blob bucket. It exposes a data
// plane (`serve`) and a control plane (`admin serve`); see docs/architecture.md.
package main

import (
	"os"

	"github.com/yolocs/open-artifact/pkg/command"
)

func main() {
	os.Exit(command.Execute())
}
