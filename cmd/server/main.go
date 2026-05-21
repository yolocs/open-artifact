// Command open-artifact is the open-artifact server: a stateless,
// multi-format artifact registry backed by a gocloud.dev/blob bucket.
//
// This is a scaffold entry point. The real command tree (cobra + viper:
// every runtime knob a flag with a matching env var, no config files) is
// introduced by the server-wiring work item. See docs/architecture.md.
package main

import (
	"fmt"
	"os"

	"github.com/yolocs/open-artifact/internal/version"
)

func main() {
	fmt.Fprintf(os.Stderr, "open-artifact %s: not yet implemented; see docs/architecture.md\n", version.Version)
	os.Exit(1)
}
