// Command core is Kiln's core (ADR 0034), built as kiln. Its commands: `kiln
// core` keeps the proxy and gives it a credential from the credential command
// (ADR 0040), ticks, gives every card that waits for a run the run of its
// stage, and serves the screen on a loopback address (ADR 0044); `kiln card`
// writes a card that waits for a run without the screen; and `kiln run`
// carries out one run started by hand, through the proxy that a core keeps;
// and `kiln launch` starts `kiln core` with its flags and starts it again
// after a crash (internal/launcher, ADR 0036). It only parses flags and the
// environment and calls internal/tick, internal/run and internal/launcher.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/TheEisbaer/kiln/internal/launcher"
	"github.com/TheEisbaer/kiln/internal/run"
	"github.com/TheEisbaer/kiln/internal/settings"
	"github.com/TheEisbaer/kiln/internal/store"
	"github.com/TheEisbaer/kiln/internal/tick"
)

// The proxy that `kiln core` keeps: its container's name, its image, and the
// ordinary network it reaches its upstream on.
const (
	proxyName    = "kiln-proxy"
	proxyImage   = "kiln-proxy"
	proxyNetwork = "kiln"
)

// The toolchain image the core builds its update in (ADR 0037, ADR 0039), and
// the run image, which the update builds as well.
const (
	toolchainImage = "kiln-toolchain"
	runImage       = "kiln-run"
)

// A run's way out (ADR 0059): its image, and the outward network it reaches
// the hosts on its list on.
const (
	wayOutImage   = "kiln-wayout"
	wayOutNetwork = "kiln-web"
)

// The variables `kiln core` reads, the same as the seal's test reads: the
// upstream, and the command that prints a credential.
const (
	upstreamVar = "KILN_CLAUDE_UPSTREAM"
	commandVar  = "KILN_CLAUDE_CREDENTIAL_COMMAND"
)

// version is the commit the program was built from, set at build with
// -ldflags "-X main.version=<commit>"; a program built by hand has none.
var version string

const usage = `usage:
  kiln launch <the flags of kiln core>   (starts kiln core with them, and starts it again after a crash)
  kiln core --data <dir> (--repo <bare repo> | --settings <file>) --model <id>   (reads ` + upstreamVar + ` and ` + commandVar + `)
            [--implement-model <id>] [--review-model <id>]
            [--image kiln-run] [--proxy kiln-proxy] [--spec-hosts <* or names, by commas>]
            [--listen 127.0.0.1:4747, a loopback address; empty serves no screen]
            [--page <file>, read on every request instead of the embedded page]
            [--runs 3, the most runs in flight at once, from 1 to 8]
            [--publish-command <command>, run with sh -c to publish a card; empty publishes none]
            [--check-result-command <command>, run with sh -c to read a card's check at the Git host; empty reads none]
            (--settings <file> holds the repos, with their commands and hosts, and takes none of --repo, --publish-command,
             --check-result-command and --spec-hosts)
  kiln card --data <dir> [--settings <file>] [--repo <name>] <text>
  kiln run --repo <bare repo> --commit <commit> --branch <branch> --run <id>
           --instruction <file> --model <id> --effort <low|medium|high> --runs <dir>
           [--image kiln-run] [--proxy kiln-proxy, which a core keeps and gives the credential]
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "launch":
		os.Exit(launcher.Main(os.Args[2:], os.Stderr))
	case "core":
		os.Exit(coreCommand(os.Args[2:], os.Getenv, os.Stdout, os.Stderr))
	case "card":
		os.Exit(cardCommand(os.Args[2:], os.Stdout, os.Stderr))
	case "run":
		os.Exit(runCommand(os.Args[2:], os.Stdout, os.Stderr))
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

// coreCommand is `kiln core`. SIGINT or SIGTERM ends it, after the following
// of every run, which goes on. Without the upstream and the credential
// command in its environment it does not start.
func coreCommand(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cfg, code := coreConfig(args, getenv, stdout, stderr)
	if code != 0 {
		return code
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := tick.Core(ctx, cfg, stdout, stderr); err != nil {
		fmt.Fprintln(stderr, "kiln core:", err)
		return 1
	}
	return 0
}

// perRepoFlags are the flags that the settings file replaces, in the order
// they are named when they are refused.
var perRepoFlags = []string{"repo", "publish-command", "check-result-command", "spec-hosts"}

// coreConfig reads the flags of `kiln core` into the core's configuration. A
// non-zero code is the exit code, with the reason already written to stderr.
// With --settings it reads the repos from the file and prints their names.
func coreConfig(args []string, getenv func(string) string, stdout, stderr io.Writer) (tick.Config, int) {
	var cfg tick.Config
	fs := flag.NewFlagSet("kiln core", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.Data, "data", "", "the instance's data directory")
	fs.StringVar(&cfg.Repo, "repo", "", "the instance's copy of the repo, a bare repo")
	settingsFile := fs.String("settings", "", "the settings file, which holds the repos; it takes none of --repo, --publish-command, --check-result-command and --spec-hosts")
	fs.StringVar(&cfg.Model, "model", "", "the model of the spec runs it starts, an exact identifier; the effort is high")
	fs.StringVar(&cfg.ImplementModel, "implement-model", "", "the model of the implement runs whose approved spec names none of claude-opus-5-5, claude-sonnet-5-5 and claude-haiku-5-5, --model by default; after a send-back an implement run uses claude-opus-5-5; the effort is medium")
	fs.StringVar(&cfg.ReviewModel, "review-model", "", "the model of the review runs it starts, --model by default; the effort is high")
	fs.StringVar(&cfg.Image, "image", runImage, "the run image")
	fs.StringVar(&cfg.Proxy, "proxy", proxyName, "the proxy container, which the core keeps")
	fs.StringVar(&cfg.Listen, "listen", "127.0.0.1:4747", "the loopback address the screen is served on; empty serves none")
	fs.StringVar(&cfg.Page, "page", "", "a file the screen's page is read from on every request instead of the embedded one, for work on the screen")
	fs.IntVar(&cfg.Runs, "runs", tick.DefaultRuns, fmt.Sprintf("the most runs in flight at once, from 1 to %d", tick.MaxRuns))
	fs.StringVar(&cfg.PublishCommand, "publish-command", "", "the developer's command that publishes a card, run with sh -c in the data directory; empty, no card is published")
	fs.StringVar(&cfg.CheckResultCommand, "check-result-command", "", "the developer's command that reads the check of a card at the Git host, run with sh -c in the data directory with KILN_COMMIT; empty, no check is read")
	hosts := fs.String("spec-hosts", "", "the hosts a spec run may reach: * for every public host, or names separated by commas; none by default")
	if err := fs.Parse(args); err != nil {
		return cfg, 2
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if fs.NArg() > 0 || cfg.Data == "" || cfg.Model == "" || given["settings"] && *settingsFile == "" ||
		cfg.Repo == "" && !given["settings"] {
		fmt.Fprint(stderr, usage)
		return cfg, 2
	}
	if given["settings"] {
		var both []string
		for _, name := range perRepoFlags {
			if given[name] {
				both = append(both, "--"+name)
			}
		}
		if len(both) > 0 {
			fmt.Fprintf(stderr, "kiln core: --settings and %s are both given; each setting has one place (ADR 0025)\n", strings.Join(both, ", "))
			return cfg, 2
		}
	}
	cfg.Upstream, cfg.CredentialCommand = getenv(upstreamVar), getenv(commandVar)
	if cfg.Upstream == "" || cfg.CredentialCommand == "" {
		fmt.Fprintf(stderr, "kiln core: set %s and %s\n", upstreamVar, commandVar)
		return cfg, 2
	}
	if given["settings"] {
		repos, err := settings.Load(*settingsFile)
		if err != nil {
			fmt.Fprintln(stderr, "kiln core:", err)
			return cfg, 1
		}
		cfg.Repos = repos
		fmt.Fprintf(stdout, "kiln core: the repos are read from %s: %s\n", *settingsFile, describe(repos))
	}
	cfg.ProxyImage, cfg.ProxyNetwork = proxyImage, proxyNetwork
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
	cfg.Version = version
	cfg.ToolchainImage, cfg.RunImage = toolchainImage, runImage
	return cfg, 0
}

// describe lists the repos' names in order, Kiln's own marked.
func describe(repos []settings.Repo) string {
	names := make([]string, len(repos))
	for i, r := range repos {
		names[i] = r.Name
		if r.Own {
			names[i] += " (Kiln's own)"
		}
	}
	return strings.Join(names, ", ")
}

// cardCommand is `kiln card`: it prints the ID of the card it wrote.
func cardCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kiln card", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", "", "the instance's data directory")
	settingsFile := fs.String("settings", "", "the settings file that names the repos; without it the one repo is kiln")
	repoName := fs.String("repo", "", "the card's repo, by name; it may be left out when there is one repo")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var given bool
	fs.Visit(func(f *flag.Flag) { given = given || f.Name == "settings" })
	if fs.NArg() != 1 || *data == "" || given && *settingsFile == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	names := []string{store.KilnRepo}
	if given {
		repos, err := settings.Load(*settingsFile)
		if err != nil {
			fmt.Fprintln(stderr, "kiln card:", err)
			return 1
		}
		names = names[:0]
		for _, r := range repos {
			names = append(names, r.Name)
		}
	}
	repo := *repoName
	switch {
	case repo != "" && !slices.Contains(names, repo):
		fmt.Fprintf(stderr, "kiln card: the settings have no repo %q; their repos: %s\n", repo, strings.Join(names, ", "))
		return 1
	case repo == "" && len(names) > 1:
		fmt.Fprintf(stderr, "kiln card: the settings have more than one repo; name one with --repo: %s\n", strings.Join(names, ", "))
		return 1
	case repo == "":
		repo = names[0]
	}
	id, err := tick.AddCard(*data, repo, fs.Arg(0))
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
