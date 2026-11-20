// Command core is Kiln's core (ADR 0034), built as kiln. Its commands: `kiln
// core` ticks, gives every card that waits for a run the run of its stage, and
// serves the screen on a loopback address (ADR 0044); `kiln card` writes a
// card that waits for a run without the screen; `kiln run` carries out one run
// started by hand; and `kiln proxy` starts the proxy with a credential from
// the credential command (ADR 0040).
// It only parses flags and calls internal/tick, internal/run and the seal.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/TheEisbaer/kiln/internal/run"
	"github.com/TheEisbaer/kiln/internal/seal"
	"github.com/TheEisbaer/kiln/internal/tick"
)

// The proxy that `kiln proxy` starts: its container's name, its image, and the
// ordinary network it reaches its upstream on.
const (
	proxyName    = "kiln-proxy"
	proxyImage   = "kiln-proxy"
	proxyNetwork = "kiln"
)

// A run's way out (ADR 0059): its image, and the outward network it reaches
// the hosts on its list on.
const (
	wayOutImage   = "kiln-wayout"
	wayOutNetwork = "kiln-web"
)

// The variables `kiln proxy` reads, the same as the seal's test reads.
const (
	upstreamVar = "KILN_CLAUDE_UPSTREAM"
	commandVar  = "KILN_CLAUDE_CREDENTIAL_COMMAND"
)

const usage = `usage:
  kiln core --data <dir> --repo <bare repo> --model <id>
            [--implement-model <id>] [--review-model <id>]
            [--image kiln-run] [--proxy kiln-proxy] [--spec-hosts <* or names, by commas>]
            [--listen 127.0.0.1:4747, a loopback address; empty serves no screen]
            [--page <file>, read on every request instead of the embedded page]
  kiln card --data <dir> <text>
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
	case "core":
		os.Exit(coreCommand(os.Args[2:], os.Stdout, os.Stderr))
	case "card":
		os.Exit(cardCommand(os.Args[2:], os.Stdout, os.Stderr))
	case "run":
		os.Exit(runCommand(os.Args[2:], os.Stdout, os.Stderr))
	case "proxy":
		os.Exit(proxyCommand(os.Getenv, os.Stderr))
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

// coreCommand is `kiln core`. SIGINT or SIGTERM ends it, after the following
// of a run, which goes on.
func coreCommand(args []string, stdout, stderr io.Writer) int {
	var cfg tick.Config
	fs := flag.NewFlagSet("kiln core", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.Data, "data", "", "the instance's data directory")
	fs.StringVar(&cfg.Repo, "repo", "", "the instance's copy of the repo, a bare repo")
	fs.StringVar(&cfg.Model, "model", "", "the model of the spec runs it starts, an exact identifier; the effort is high")
	fs.StringVar(&cfg.ImplementModel, "implement-model", "", "the model of the implement runs it starts, --model by default; the effort is medium")
	fs.StringVar(&cfg.ReviewModel, "review-model", "", "the model of the review runs it starts, --model by default; the effort is high")
	fs.StringVar(&cfg.Image, "image", "kiln-run", "the run image")
	fs.StringVar(&cfg.Proxy, "proxy", proxyName, "the proxy container")
	fs.StringVar(&cfg.Listen, "listen", "127.0.0.1:4747", "the loopback address the screen is served on; empty serves none")
	fs.StringVar(&cfg.Page, "page", "", "a file the screen's page is read from on every request instead of the embedded one, for work on the screen")
	hosts := fs.String("spec-hosts", "", "the hosts a spec run may reach: * for every public host, or names separated by commas; none by default")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 || cfg.Data == "" || cfg.Repo == "" || cfg.Model == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	if *hosts != "" {
		cfg.Hosts = strings.Split(*hosts, ",")
	}
	if cfg.ImplementModel == "" {
		cfg.ImplementModel = cfg.Model
	}
	if cfg.ReviewModel == "" {
		cfg.ReviewModel = cfg.Model
	}
	cfg.WayOutImage, cfg.WayOutNetwork = wayOutImage, wayOutNetwork
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := tick.Core(ctx, cfg, stdout, stderr); err != nil {
		fmt.Fprintln(stderr, "kiln core:", err)
		return 1
	}
	return 0
}

// cardCommand is `kiln card`: it prints the ID of the card it wrote.
func cardCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kiln card", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", "", "the instance's data directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 || *data == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	id, err := tick.AddCard(*data, fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "kiln card:", err)
		return 1
	}
	fmt.Fprintln(stdout, id)
	return 0
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
