// Command xt copies files between machines over ssh, rsync style.
package main

import (
	"fmt"
	"os"

	"github.com/gurupras/xt/internal/opt"
	"github.com/gurupras/xt/internal/proto"
	"github.com/gurupras/xt/internal/xfer"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--server" {
		os.Exit(xfer.Serve(os.Stdin, os.Stdout))
	}
	o, err := opt.Parse(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "xt: %v\nTry 'xt --help' for more information.\n", err)
		os.Exit(xfer.ExitUsage)
	}
	if o.Help {
		fmt.Print(opt.Usage)
		return
	}
	if o.Version {
		fmt.Printf("xt %s (protocol %d)\n", version, proto.Version)
		return
	}
	os.Exit(xfer.RunClient(o))
}
