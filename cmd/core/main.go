// Command core is Kiln's core (ADR 0034), built as kiln. For now it has two
// commands: `kiln run` carries out one run started by hand, and `kiln proxy`
// starts the proxy with a credential from the credential command (ADR 0040).
// It only parses flags and calls internal/run and the seal.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/TheEisbaer/kiln/internal/run"
	"github.com/TheEisbaer/kiln/internal/seal"
)

// The proxy that `kiln proxy` starts: its container's name, its image, and the
// ordinary network it reaches its upstream on.
const (
	proxyName    = "kiln-proxy"
	proxyImage   = "kiln-proxy"
	proxyNetwork = "kiln"
)

// The variables `kiln proxy` reads, the same as the seal's test reads.
const (
	upstreamVar = "KILN_CLAUDE_UPSTREAM"
	commandVar  = "KILN_CLAUDE_CREDENTIAL_COMMAND"
)

const usage = `usage:
  kiln run --repo <bare repo> --commit <commit> --branch <branch> --run <id>
           --instruction <file> --model <id> --effort <low|medium|high> --runs <dir>
           [--image kiln-run] [--proxy kiln-proxy]
  kiln proxy   (reads ` + upstreamVar + ` and ` + commandVar + `)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		os.Exit(runCommand(os.Args[2:], os.Stdout, os.Stderr))
	case "proxy":
		os.Exit(proxyCommand(os.Getenv, os.Stderr))
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

// runCommand is `kiln run`. Ctrl-C ends the following only: the run goes on.
func runCommand(args []string, stdout, stderr io.Writer) int {
	in, err := parseRun(args, stderr)
	if err != nil {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return run.Do(ctx, in, stdout, stderr)
}

// parseRun reads the flags of `kiln run`.
func parseRun(args []string, stderr io.Writer) (run.Inputs, error) {
	var in run.Inputs
	fs := flag.NewFlagSet("kiln run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&in.Repo, "repo", "", "the instance's copy of the repo, a bare repo")
	fs.StringVar(&in.Commit, "commit", "", "the commit the run starts from")
	fs.StringVar(&in.Branch, "branch", "", "the branch the run's change is saved to")
	fs.StringVar(&in.ID, "run", "", "the run's ID")
	fs.StringVar(&in.Instruction, "instruction", "", "the file of the instruction")
	fs.StringVar(&in.Model, "model", "", "the model, an exact identifier")
	fs.StringVar(&in.Effort, "effort", "", "the effort: low, medium or high")
	fs.StringVar(&in.Runs, "runs", "", "the directory of the runs' records")
	fs.StringVar(&in.Image, "image", "kiln-run", "the run image")
	fs.StringVar(&in.Proxy, "proxy", proxyName, "the proxy container")
	if err := fs.Parse(args); err != nil {
		return in, err
	}
	if fs.NArg() > 0 {
		err := fmt.Errorf("unexpected arguments: %q", fs.Args())
		fmt.Fprintln(stderr, "kiln run:", err)
		return in, err
	}
	return in, nil
}

// proxyCommand is `kiln proxy`: it starts the proxy, replacing one there is.
func proxyCommand(getenv func(string) string, stderr io.Writer) int {
	upstream, command := getenv(upstreamVar), getenv(commandVar)
	if upstream == "" || command == "" {
		fmt.Fprintf(stderr, "kiln proxy: set %s and %s\n", upstreamVar, commandVar)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := seal.ReplaceProxy(ctx, seal.Proxy{Name: proxyName, Image: proxyImage, Network: proxyNetwork,
		Upstream: upstream}, command); err != nil {
		fmt.Fprintln(stderr, "kiln proxy:", err)
		return 1
	}
	return 0
}
