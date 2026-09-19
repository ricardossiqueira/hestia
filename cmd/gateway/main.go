// Command iot-gateway will run the local IoT gateway on the Orange Pi.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ricar/iot-gateway/internal/config"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "validate" {
		fmt.Fprintln(stderr, "usage: iot-gateway validate --config <path>")
		return 2
	}

	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "config/gateway.yaml", "path to the YAML configuration file")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: iot-gateway validate --config <path>")
		return 2
	}

	if _, err := config.Load(*configPath); err != nil {
		fmt.Fprintf(stderr, "configuration is invalid: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "configuration is valid")
	return 0
}
