# Getting started on your own Mac

This guide takes you from nothing to a card of yours published on your team's repo, on a Mac with Apple silicon. You need Homebrew, your own Claude sign-in, your own sign-in at your team's Git host (Azure DevOps or another), and the address of your team's repo. Nothing here uses anyone else's machine, files or credentials.

Kiln runs on your Mac as one program, the core, with a container engine beside it. You write cards on its screen, in the browser. Your instance holds two repos: Kiln's own, only so that Kiln can update itself, and your team's repo, the one you work on. Your own working clone of the team repo is never used or touched.

Every step ends with a **Check:** that says what to run or look at and what it must show. Do not go on until it does. Run each block in a terminal of your own, in Terminal or iTerm, not inside a Claude Code session. Each block reads your values first, so it also works in a new terminal window. The `<!-- kiln-install: … -->` lines in this file's source mark the blocks for the check that tests this guide; GitHub does not show them.

[README.md](../README.md) is the reference: this guide links to it and does not repeat it.

## 0. Your values

Every later block reads its values from one file, `~/kiln/values.sh`. Run this block once. It writes the file with examples, and leaves a file that is there as it is.

<!-- kiln-install: values -->
```sh
mkdir -p "$HOME/kiln"
[ -e "$HOME/kiln/values.sh" ] || {
cat >"$HOME/kiln/values.sh.new" <<'EOF'
# The values of docs/getting-started.md. No secret goes here: a credential is
# named only by the command that reads it.

# Where your instance lives: Kiln's source, the copies, the data, the settings.
KILN=$HOME/kiln
# Where Kiln comes from, and where your instance updates itself from.
KILN_GIT=https://github.com/TheEisbaer/kiln.git
# Your team's repo, its https address as you would clone it.
TEAM_REPO=https://dev.azure.com/your-org/your-project/_git/your-repo
# The name Kiln shows for it: letters, digits and -.
REPO_NAME=team
# The most cards in progress at once, for the instance and for the repo.
LIMIT=1
# required when the repo's pull requests cannot merge without a linked work
# item, as on Azure DevOps with that policy; empty otherwise.
WORK_ITEM=required
# The hosts a spec run may read on the web: * is every public host; names
# separated by commas are those hosts only; empty is none, and then every spec
# waits for you as written without the web. Runs never reach your Mac or your
# network, only public hosts.
SPEC_HOSTS='*'
# The commands that publish a card and read its check (step 7).
PUBLISH=$KILN/source/scripts/azure-devops/publish
CHECK_RESULT="$KILN/source/scripts/azure-devops/check-result $KILN/team.git"
# Claude's upstream and the command that prints your credential (step 5).
# Both empty: Kiln reads them from ~/.claude/settings.json.
CLAUDE_UPSTREAM=
CLAUDE_CREDENTIAL_COMMAND=
EOF
mv "$HOME/kiln/values.sh.new" "$HOME/kiln/values.sh"
}
```

Then open `~/kiln/values.sh` in an editor, for example with `open -e ~/kiln/values.sh`, and set `TEAM_REPO`, `REPO_NAME` and `LIMIT`. Leave the rest until the step that names it.

**Check:** `cat ~/kiln/values.sh` shows your team repo's address on the line `TEAM_REPO=`.

## 1. Tools

Install the container engine, the Docker command and its build plugin, and two tools Kiln uses. On Azure DevOps, also the Azure CLI and gitleaks, which the publish command scans every change with before it leaves your Mac.

<!-- kiln-install: mac -->
```sh
brew install colima docker docker-buildx jq git
# docker-buildx asks for this entry. Without it, docker build falls back to the
# legacy builder, and the run image's build stops (see the end of this guide).
mkdir -p "$HOME/.docker"
[ -f "$HOME/.docker/config.json" ] || echo '{}' >"$HOME/.docker/config.json"
jq --arg dir "$(brew --prefix)/lib/docker/cli-plugins" \
    '.cliPluginsExtraDirs = ((.cliPluginsExtraDirs // []) + [$dir] | unique)' \
    "$HOME/.docker/config.json" >"$HOME/.docker/config.json.new" &&
    mv "$HOME/.docker/config.json.new" "$HOME/.docker/config.json"
```

<!-- kiln-install: azure -->
```sh
brew install azure-cli gitleaks
```

**Check:** this block prints three versions and no error.

<!-- kiln-install: check -->
```sh
docker buildx version
jq --version
git --version
```

## 2. The container engine

Every run of Kiln is a container. Each takes the repo's `memory`, 4 GiB by default, and its `cpus`, 2 by default, and up to `LIMIT` of them run at once. So the engine needs at least `LIMIT × 2` CPUs and `LIMIT × 4 + 2` GiB of memory: 2 CPUs and 6 GiB for a `LIMIT` of 1. colima's own default, 2 GiB, is too small.

<!-- kiln-install: mac -->
```sh
. "$HOME/kiln/values.sh"
colima start --cpu "$((LIMIT * 2))" --memory "$((LIMIT * 4 + 2))" --disk 100
```

To change the size later, run `colima stop` and then this block again with other values. colima shares only your home directory with the engine, and Kiln builds its program from a folder it shares with the engine, so keep everything of Kiln under your home directory, as these values do.

**Check:** this block prints `the engine is large enough`. A VM shows a little less memory than it was given, so 90% of it counts.

<!-- kiln-install: check -->
```sh
. "$HOME/kiln/values.sh"
cpus=$(docker info --format '{{.NCPU}}')
mem=$(docker info --format '{{.MemTotal}}')
[ "$cpus" -ge "$((LIMIT * 2))" ] ||
    { echo "the engine has $cpus CPUs; give it at least $((LIMIT * 2))"; false; }
[ "$((mem / 1048576))" -ge "$(((LIMIT * 4 + 2) * 1024 * 9 / 10))" ] ||
    { echo "the engine has $((mem / 1048576)) MiB; give it at least $((LIMIT * 4 + 2)) GiB"; false; }
echo "the engine is large enough"
```

## 3. Get Kiln

Kiln's source, which `kiln-up` builds the program and the images from, and whose Azure DevOps commands your instance runs.

<!-- kiln-install: run -->
```sh
. "$HOME/kiln/values.sh"
[ -d "$KILN/source" ] || git clone "$KILN_GIT" "$KILN/source"
```

Kiln's updates do not move this checkout. To bring its commands up to date later, run `git -C ~/kiln/source pull` while Kiln is stopped.

**Check:** this block prints the newest commit of Kiln's source.

<!-- kiln-install: check -->
```sh
. "$HOME/kiln/values.sh"
[ -f "$KILN/source/go.mod" ] || { echo "$KILN/source is not Kiln's source"; false; }
git -C "$KILN/source" log -1 --oneline
```

## 4. The instance's directory and its copies

The instance keeps its data in one directory and works on bare copies of the two repos, which it fetches `main` into by itself. A fetch must ask nothing, since nobody is there to answer it.

On Azure DevOps, use Git Credential Manager, which signs you in once in the browser and then answers every fetch and push. Use the https address of your repo: the Azure DevOps commands refuse one over ssh.

<!-- kiln-install: azure -->
```sh
brew install --cask git-credential-manager
```

On another Git host, use what it offers for https without a prompt, or an ssh key in an agent.

<!-- kiln-install: run -->
```sh
. "$HOME/kiln/values.sh"
mkdir -p "$KILN/data"
[ -e "$KILN/kiln.git" ] || git clone --bare "$KILN_GIT" "$KILN/kiln.git"
[ -e "$KILN/team.git" ] || git clone --bare "$TEAM_REPO" "$KILN/team.git"
```

A clone that was stopped halfway leaves a directory that this block then skips, and the check below fails: remove that directory and run the block again.

**Check:** this block prints `the copies are ready`. Each copy is bare, its `HEAD` is `main`, and a fetch succeeds without a prompt.

<!-- kiln-install: check -->
```sh
. "$HOME/kiln/values.sh"
for copy in "$KILN/kiln.git" "$KILN/team.git"; do
    [ "$(git -C "$copy" rev-parse --is-bare-repository)" = true ] ||
        { echo "$copy is not a bare repository"; false; }
    [ "$(git -C "$copy" symbolic-ref HEAD)" = refs/heads/main ] ||
        { echo "the HEAD of $copy is not main"; false; }
    GIT_TERMINAL_PROMPT=0 git -C "$copy" fetch --quiet origin main ||
        { echo "fetching main into $copy failed or asked for a password"; false; }
done
echo "the copies are ready"
```

## 5. Claude's sign-in, your own

Kiln runs Claude Code in its containers, and they reach Claude only through Kiln's proxy, which holds your credential. Kiln takes two things: the upstream, Claude's API or a service in front of it, and a command that prints your credential. Kiln holds only what the command prints, and never writes it anywhere. The credential is yours: never use someone else's. The contract is README's step 4 ([Starting it](../README.md#starting-it)). Take the route that fits your setup.

**(a) Your Claude Code already uses a key helper and a base URL**, as a team with a service in front of Claude sets up. Leave `CLAUDE_UPSTREAM` and `CLAUDE_CREDENTIAL_COMMAND` empty: `kiln-up` reads `.env.ANTHROPIC_BASE_URL` and `.apiKeyHelper` from `~/.claude/settings.json`. This shows both; neither line may be `null`:

<!-- kiln-install: mac -->
```sh
jq -r '.apiKeyHelper, .env.ANTHROPIC_BASE_URL' "$HOME/.claude/settings.json"
```

**(b) A Claude Console API key**, billed per token to your Console account, not covered by a Pro or Max subscription. Put the key into your login Keychain. The command asks for it, so it never sits in a file or in your shell's history:

<!-- kiln-install: mac -->
```sh
security add-generic-password -a "$USER" -s kiln-claude -w
```

Then set these two lines in `~/kiln/values.sh`:

<!-- kiln-install: mac -->
```sh
CLAUDE_UPSTREAM=https://api.anthropic.com
CLAUDE_CREDENTIAL_COMMAND='security find-generic-password -s kiln-claude -w'
```

A subscription sign-in alone, without (a) or (b), is not covered by this guide yet.

**Check:** this block prints how many bytes your credential command printed, more than 1, and never the credential itself.

<!-- kiln-install: check -->
```sh
. "$HOME/kiln/values.sh"
up=$CLAUDE_UPSTREAM cmd=$CLAUDE_CREDENTIAL_COMMAND
[ -n "$up" ] || up=$(jq -r '.env.ANTHROPIC_BASE_URL // empty' "$HOME/.claude/settings.json")
[ -n "$cmd" ] || cmd=$(jq -r '.apiKeyHelper // empty' "$HOME/.claude/settings.json")
[ -n "$up" ] && [ -n "$cmd" ] || { echo "no upstream or no credential command: see route (a) or (b)"; false; }
n=$(sh -c "$cmd" </dev/null | wc -c | tr -d ' ')
[ "$n" -gt 1 ] || { echo "the credential command printed nothing"; false; }
echo "the credential command printed $n bytes"
```

## 6. The team repo's toolchain, if it needs one

Kiln's runs come with Go, Node, Python and the usual tools. A repo whose build or tests need others brings its own toolchain: a Dockerfile you keep beside the settings file, never in the repo, and named in the repo's `toolchain` member. Skip this step if your repo needs nothing else. What the Dockerfile must provide, and how a repo's package caches work offline, are in README's [A repo's own toolchain](../README.md#a-repos-own-toolchain) and [A repo's package caches](../README.md#a-repos-package-caches).

To add one, put it into the team repo's object in step 8's block, before you run it:

<!-- kiln-install: mac -->
```sh
toolchain: {dockerfile: ($kiln + "/team-toolchain.Dockerfile"), inputs: ["package-lock.json"]},
```

**Check:** once Kiln runs (step 9), the core prints the repo's first build of its toolchain and that it ended, and the repo's first card starts a run.

## 7. Publishing and the check result

Kiln publishes a card by running your own publish command, and follows its pull request by running your check result command. Kiln itself holds no key and no token for the Git host.

**On Azure DevOps**, Kiln ships both commands, and the values already name them. Sign in with your own account, and add the extension the commands use:

<!-- kiln-install: azure -->
```sh
az login
az extension add --name azure-devops
```

Keep `WORK_ITEM=required` if your repo's pull requests cannot merge without a linked work item, which the shipped publish command needs anyway. What the commands need and do is README's "On Azure DevOps" in [Starting it](../README.md#starting-it).

**Check:** this block prints `pending`: there is no pull request of `main`'s head.

<!-- kiln-install: azure -->
```sh
. "$HOME/kiln/values.sh"
last=$(KILN_COMMIT=$(git -C "$KILN/team.git" rev-parse main) KILN_SINCE=1970-01-01T00:00:00Z sh -c "$CHECK_RESULT" | tail -n 1)
[ "$last" = pending ] || { echo "the check result command printed $last"; false; }
echo "$last"
```

**On another Git host**, write the two commands yourself, keep them outside every repo, and set `PUBLISH` and `CHECK_RESULT` to them; leave `WORK_ITEM` empty unless your command links work items. The publish command gets the card's commit and branch, pushes it and prints the pull request's address last. The check result command gets a commit and prints `pending`, `passed`, `failed`, `closed` or `review` last. Their contracts are `--publish-command` and `--check-result-command` in README's [Starting it](../README.md#starting-it).

## 8. The settings file

The settings file names the instance's repos and their commands. It holds commands, so it stays outside every repo, with mode 0600.

<!-- kiln-install: run -->
```sh
. "$HOME/kiln/values.sh"
jq -n --arg kiln "$KILN" --arg name "$REPO_NAME" --argjson limit "$LIMIT" \
      --arg pub "$PUBLISH" --arg check "$CHECK_RESULT" \
      --arg item "$WORK_ITEM" --arg hosts "$SPEC_HOSTS" \
    '{limit: $limit,
      repos: [{name: "kiln", copy: ($kiln + "/kiln.git"), own: true},
              ({name: $name, copy: ($kiln + "/team.git"), limit: $limit,
                publish_command: $pub, check_result_command: $check}
               + (if $item == "" then {} else {work_item: $item} end)
               + (if $hosts == "" then {} else {spec_hosts: ($hosts | split(","))} end))]}' \
    >"$KILN/settings.json.new"
chmod 600 "$KILN/settings.json.new"
mv "$KILN/settings.json.new" "$KILN/settings.json"
```

Kiln's own repo is there only so that Kiln can update itself: it has no commands, and you write no cards for it. Every member is in README's step 4; Kiln refuses a member it does not know, at its start (step 9).

**Check:** this block prints the file, with two repos.

<!-- kiln-install: check -->
```sh
. "$HOME/kiln/values.sh"
jq -e '.repos | length == 2' "$KILN/settings.json" >/dev/null ||
    { echo "$KILN/settings.json does not hold two repos"; false; }
jq . "$KILN/settings.json"
```

## 9. Start

`kiln-up` builds the images and the program, and starts Kiln. The first start builds four images and the program, and downloads Claude Code: it takes several minutes. Later starts build only what changed. It prints what it does, and then the core prints each run. Ctrl-C stops Kiln; to start again, run the same block.

<!-- kiln-install: start -->
```sh
. "$HOME/kiln/values.sh"
KILN_DATA="$KILN/data" KILN_SETTINGS="$KILN/settings.json" \
    KILN_CLAUDE_UPSTREAM="$CLAUDE_UPSTREAM" KILN_CLAUDE_CREDENTIAL_COMMAND="$CLAUDE_CREDENTIAL_COMMAND" \
    "$KILN/source/scripts/kiln-up"
```

Once it is up, the screen opens by itself at <http://127.0.0.1:4747/>.

**Check:** the screen shows both repos, `kiln` and yours, and its top does not say "Paused". From a second terminal, this block prints `Kiln is up`.

<!-- kiln-install: check -->
```sh
. "$HOME/kiln/values.sh"
curl -fsS http://127.0.0.1:4747/api/board |
    jq -e --arg name "$REPO_NAME" '[.repos[].name] == ["kiln", $name] and .paused == ""' >/dev/null ||
    { echo "the screen does not answer, does not show both repos, or says Paused"; false; }
echo "Kiln is up"
```

## 10. A first card, to merged

On the screen:

1. Write a small card for your repo, such as "Add one sentence to the README that says what the repo is for." Pick your repo and post the card. It starts at once: its tile moves to Spec, and the core prints the spec run.
2. When the spec run ends, the card waits for you under "needs you". Open it and read the spec. If it is right, approve it, with the work item number when your repo needs one. Otherwise send it back with a note.
3. Implement and review then run by themselves, one after the other. The tile shows the stage it is in.
4. When the review passes, Kiln publishes the card with your publish command, and the card waits for the Git host. Its page has the pull request's address. Open it there, and let your team review, approve and merge it as any pull request.
5. A comment of a reviewer, or a failed check, brings the card back under "needs you" with what was said. Once the pull request has merged, Kiln sees it in `main` and the card is done.

**Check:** the card's tile is under Done, as merged.

## 11. Stop

Press Ctrl-C in the terminal where Kiln runs, then stop the proxy:

<!-- kiln-install: run -->
```sh
. "$HOME/kiln/values.sh"
"$KILN/source/scripts/kiln-down"
```

To start again, run step 9's block. Kiln updates itself after each merge to Kiln's own repo, while it runs: README's step 4 says how.

**Check:** this block prints `the proxy is stopped`.

<!-- kiln-install: check -->
```sh
[ -z "$(docker ps --filter name=kiln-proxy --format '{{.Status}}')" ] ||
    { echo "the proxy still runs"; false; }
echo "the proxy is stopped"
```

## What goes wrong most often, and how it shows

- `kiln-up: the container engine does not answer`: colima is not running. Run step 2's block.
- `no Claude Code pinned for the architecture ''` while the run image builds: `docker build` has no buildx. Do step 1's config entry and check it.
- A card waits for a run and the core prints that the run asks for more CPUs than the engine has, or a run is killed for its memory: the engine is too small for the repo's runs. Resize it as in step 2.
- The program's build finds no `go.mod`: Kiln's source is outside your home directory, which colima does not share. Keep `KILN` under your home directory.
- "Paused" at the top of the screen, with the credential command's error: the command fails, as when the Keychain is locked or its item is missing. Run step 5's check.
- "Paused", saying that the upstream refused the credential: the key is wrong or expired. Put the right one in, as in step 5.
- A fetch that asks for a password, or an origin over ssh, which the Azure DevOps commands refuse: set up Git Credential Manager and the https address, as in step 4.
- The copy's `HEAD` is not `main`: Kiln works only on `main`. Step 4's check says so.
- The card waits in review for a work item number: give it one on the card's page, and it is published.
- The card needs you with an error of `az`: `az` is not signed in, or has no `azure-devops` extension. Do step 7 again.
- The card needs you because gitleaks found something: nothing was pushed. Read the finding on the card's page; send the card back with a note, or drop it.
- `kiln-up: refusing to start inside a Claude Code session`: start it from a terminal of your own.
- The screen does not open, and the core says the address is in use: something else listens on port 4747. Stop it, or a Kiln that is still running.
- The core refuses to start and names a member of the settings file it does not know: fix its name in step 8's block, and run it again.
