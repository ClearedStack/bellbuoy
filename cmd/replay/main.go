// Command replay reads saved items from standard input, one JSON item per
// line, runs them through the internal-git sub-tool against an in-memory
// task store, and prints what happened as JSON.
//
// It is a way to try the first milestone by hand. It is not the Bellbuoy
// worker: it does not talk to Muster, Plainforge or anything else.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/clearedstack/bellbuoy/internal/replay"
)

func main() {
	rep, err := replay.Run(context.Background(), os.Stdin)
	if err == nil {
		var out []byte
		if out, err = rep.JSON(); err == nil {
			_, err = os.Stdout.Write(out)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "replay: could not read the input or write the report")
		os.Exit(1)
	}
}
