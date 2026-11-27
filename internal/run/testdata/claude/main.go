// Command claude stands in for Claude in the run test. It answers Claude Code's
// Messages requests from a script: a Bash call `sh run.sh` first, then the text
// "Done." once a tool result has come. A case whose word starts with
// kiln-case-web has a script of four steps instead (ADR 0059): the Bash call,
// then a WebFetch of the fake public host's page, then the answer to
// WebFetch's own call, a request without tools that holds the page, which is
// the page's marker, and "Done." once two tool results have come. The case
// kiln-case-tick-spec has three steps: the Bash call, then a Write of the spec
// to /kiln/out/spec.md, then "Done.". The case kiln-case-tick-refused has the
// same three steps, and refuses the credential after the case's first answer:
// a Messages request whose x-api-key ends in -refused gets 401, as Claude's
// API answers a credential it does not take, until the core has pushed
// another (ADR 0040). The case kiln-case-install, of check's fresh-install
// test, tells the stage by the heading of the instruction in the request, and
// answers one Bash call that writes the stage's documents, and for implement
// a line in NOTES.md, and then "Done.". Its data follows the Claude Code
// version pinned in images/run/Dockerfile; the card that changes the pin
// updates it.
//
// For each request it writes one line of JSON to standard output: the model,
// output_config.effort, the x-api-key header, whether the marker given as its
// argument is in the body, and the case named in the body, a word
// kiln-case-<name>, by which the test tells its runs apart. So that a request
// the test did not expect explains itself, the line also has the time, the
// client's address, Claude Code's session and the retry count of its SDK
// (headers X-Claude-Code-Session-Id and X-Stainless-Retry-Count, read from a
// request of 2.1.326), and the shape of the body. After the answer it writes a
// second line, of the kind "answered", with how long the answer took and
// whether writing it failed.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// port is where it listens, inside its container.
const port = ":8081"

// caseWord names a test case in an instruction.
var caseWord = regexp.MustCompile(`kiln-case-[a-z0-9-]+`)

// request is what is read of a Messages request.
type request struct {
	Model        string            `json:"model"`
	Stream       bool              `json:"stream"`
	MaxTokens    int               `json:"max_tokens"`
	Tools        []json.RawMessage `json:"tools"`
	System       json.RawMessage   `json:"system"`
	OutputConfig struct {
		Effort string `json:"effort"`
	} `json:"output_config"`
	Messages []struct {
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// logLine is what is written per request, of the kind "request".
type logLine struct {
	Kind       string `json:"kind"`
	Time       string `json:"time"`
	Remote     string `json:"remote"`
	Session    string `json:"session"`
	RetryCount string `json:"retry_count"`
	Stream     bool   `json:"stream"`
	Messages   int    `json:"messages"`
	MaxTokens  int    `json:"max_tokens"`
	Tools      int    `json:"tools"`
	SystemSize int    `json:"system_bytes"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Model      string `json:"model"`
	Effort     string `json:"effort"`
	APIKey     string `json:"x_api_key"`
	Marker     bool   `json:"marker"`
	Case       string `json:"case"`
	ToolResult bool   `json:"tool_result"`
	Answer     string `json:"answer"` // the step of the script: bash, webfetch, page, write, done or refused
}

// refusedCase is the case whose credential is refused after its first
// answer, and refusedKey the end of the credential that is refused.
const (
	refusedCase = "kiln-case-tick-refused"
	refusedKey  = "-refused"
)

// refusedBody is the answer to a refused credential, as Claude's API gives it.
const refusedBody = `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`

func main() {
	if len(os.Args) != 2 || os.Args[1] == "" {
		log.Fatal("usage: claude <marker>")
	}
	marker := os.Args[1]
	out := json.NewEncoder(os.Stdout)
	var mu sync.Mutex
	answered := map[string]bool{} // the cases with a Messages request answered
	log.Fatal(http.ListenAndServe(port, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req request
		_ = json.Unmarshal(body, &req)
		results := toolResults(req)
		toolResult := results > 0
		start := time.Now()
		caseName := caseWord.FindString(string(body))
		block, stop, step := script(req, string(body), caseName, results)
		refuse := false
		if r.Method == http.MethodPost && r.URL.Path == "/v1/messages" && caseName == refusedCase {
			mu.Lock()
			refuse = answered[caseName] && strings.HasSuffix(r.Header.Get("X-Api-Key"), refusedKey)
			answered[caseName] = true
			mu.Unlock()
		}
		if refuse {
			step = "refused"
		}
		out.Encode(logLine{
			Kind:       "request",
			Time:       start.UTC().Format(time.RFC3339Nano),
			Remote:     r.RemoteAddr,
			Session:    r.Header.Get("X-Claude-Code-Session-Id"),
			RetryCount: r.Header.Get("X-Stainless-Retry-Count"),
			Stream:     req.Stream,
			Messages:   len(req.Messages),
			MaxTokens:  req.MaxTokens,
			Tools:      len(req.Tools),
			SystemSize: len(req.System),
			Method:     r.Method,
			Path:       r.URL.Path,
			Model:      req.Model,
			Effort:     req.OutputConfig.Effort,
			APIKey:     r.Header.Get("X-Api-Key"),
			Marker:     strings.Contains(string(body), marker),
			Case:       caseName,
			ToolResult: toolResult,
			Answer:     step,
		})

		w.Header().Set("Content-Type", "application/json")
		var err error
		switch {
		case refuse:
			w.WriteHeader(http.StatusUnauthorized)
			_, err = w.Write([]byte(refusedBody))
		case r.Method != http.MethodPost:
			_, err = w.Write([]byte("{}"))
		case strings.HasSuffix(r.URL.Path, "/count_tokens"):
			_, err = w.Write([]byte(`{"input_tokens":10}`))
		case r.URL.Path == "/v1/messages":
			err = answer(w, req, block, stop)
		default:
			http.NotFound(w, r)
		}
		answered := map[string]any{"kind": "answered", "case": caseName, "remote": r.RemoteAddr,
			"time": time.Now().UTC().Format(time.RFC3339Nano), "ms": time.Since(start).Milliseconds()}
		if err != nil {
			answered["error"] = err.Error()
		}
		out.Encode(answered)
	})))
}

// toolResults counts the tool results a request carries.
func toolResults(req request) int {
	n := 0
	for _, m := range req.Messages {
		var blocks []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue // a plain string
		}
		for _, b := range blocks {
			if b.Type == "tool_result" {
				n++
			}
		}
	}
	return n
}

// The scripted answers. The text carries an escape sequence, which the test
// expects in the record and not on the terminal.
const (
	bashInput = `{"command":"sh run.sh","description":"Run the script"}`
	doneText  = "Done.\x1b[2K"
	// The page's address on the fake public host. The prompt carries the
	// case's word, so that WebFetch's own call is told apart.
	fetchURL = "https://public.kiln-test.example/page"
	// The spec of the case kiln-case-tick-spec, which the tick test expects in
	// the store.
	specText = "# Spec of the scripted run\n\n## 1. What will be built\n\nNothing; kiln-case-tick-spec.\n"
)

// installCase is the case of check's fresh-install test (check/install_test.go).
const installCase = "kiln-case-install"

// installStage is the stage whose instruction a request of installCase holds,
// by the instruction's heading (instructions/), or "" for none. Review is
// asked first and spec last, since a later stage's instruction quotes the
// approved spec.
func installStage(body string) string {
	for _, s := range []struct{ heading, stage string }{
		{"# Review a change", "review"},
		{"# Implement a card", "implement"},
		{"# Write the spec for a card", "spec"},
	} {
		if strings.Contains(body, s.heading) {
			return s.stage
		}
	}
	return ""
}

// installCommands are the Bash calls of installCase, by stage: each writes
// what its stage's instruction asks for, and implement also the line that the
// test finds on the Git host's main once the card has merged.
var installCommands = map[string]string{
	"spec": `cat >/kiln/out/spec.md <<'EOF'
# Spec: add a line to NOTES.md

Pull request title: ` + "`docs: add a line to NOTES.md`" + `

## 1. What will be built

One line at the end of NOTES.md.

## 2. How to tell it works

NOTES.md ends with the line.

## 3. What was found, the options considered and the one chosen

Nothing to choose.

## 4. What is deliberately left out

Everything else.

## 5. Questions for the developer

None.
EOF`,
	"implement": `printf '%s\n' 'A line from the fresh install.' >>/work/NOTES.md &&
printf '%s\n' '# Report' '' 'Added the line to NOTES.md.' >/kiln/out/report.md &&
printf '%s\n' '## Why the change' '' 'NOTES.md gets a line.' '' '## Special things to note' '' '- None.' '' '## Change outline' '' 'One line.' >/kiln/out/pull-request.md`,
	"review": `printf '%s\n' 'Verdict: passes' '' 'The change adds the line the spec asks for.' >/kiln/out/review.md`,
}

// pageMarker is the marker of the fake public host's page.
var pageMarker = regexp.MustCompile(`kiln-page-[a-z0-9-]+`)

// script returns the next answer's content block, its stop reason, and the
// step's name.
func script(req request, body, caseName string, results int) (map[string]any, string, string) {
	bash := map[string]any{"type": "tool_use", "id": "toolu_kiln_1", "name": "Bash", "input": json.RawMessage(bashInput)}
	done := map[string]any{"type": "text", "text": doneText}
	if caseName == "kiln-case-tick-spec" || caseName == refusedCase {
		switch results {
		case 0:
			return bash, "tool_use", "bash"
		case 1:
			input, _ := json.Marshal(map[string]string{"file_path": "/kiln/out/spec.md", "content": specText})
			return map[string]any{"type": "tool_use", "id": "toolu_kiln_2", "name": "Write", "input": json.RawMessage(input)}, "tool_use", "write"
		default:
			return done, "end_turn", "done"
		}
	}
	if caseName == installCase {
		command := installCommands[installStage(body)]
		if results > 0 || command == "" {
			return done, "end_turn", "done"
		}
		input, _ := json.Marshal(map[string]string{"command": command, "description": "Write the stage's documents"})
		return map[string]any{"type": "tool_use", "id": "toolu_kiln_1", "name": "Bash", "input": json.RawMessage(input)}, "tool_use", "bash"
	}
	if !strings.HasPrefix(caseName, "kiln-case-web") {
		if results > 0 {
			return done, "end_turn", "done"
		}
		return bash, "tool_use", "bash"
	}
	if marker := pageMarker.FindString(body); len(req.Tools) == 0 && marker != "" {
		return map[string]any{"type": "text", "text": "The page's marker is " + marker + "."}, "end_turn", "page"
	}
	switch results {
	case 0:
		return bash, "tool_use", "bash"
	case 1:
		input, _ := json.Marshal(map[string]string{"url": fetchURL, "prompt": "Give the page's marker. " + caseName})
		return map[string]any{"type": "tool_use", "id": "toolu_kiln_2", "name": "WebFetch", "input": json.RawMessage(input)}, "tool_use", "webfetch"
	default:
		return done, "end_turn", "done"
	}
}

// answer writes an answer, streamed or whole, and returns the first error of
// writing it.
func answer(w http.ResponseWriter, req request, block map[string]any, stop string) error {
	usage := map[string]int{"input_tokens": 100, "output_tokens": 10,
		"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}
	message := map[string]any{"id": "msg_kiln", "type": "message", "role": "assistant", "model": req.Model,
		"stop_sequence": nil, "usage": usage}

	if !req.Stream {
		message["content"] = []any{block}
		message["stop_reason"] = stop
		return json.NewEncoder(w).Encode(message)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	var err error
	event := func(name string, data any) {
		b, _ := json.Marshal(data)
		if _, e := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b); e != nil && err == nil {
			err = e
		}
		if e := http.NewResponseController(w).Flush(); e != nil && err == nil {
			err = e
		}
	}
	message["content"] = []any{}
	message["stop_reason"] = nil
	event("message_start", map[string]any{"type": "message_start", "message": message})
	if block["type"] == "text" {
		event("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""}})
		event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": block["text"]}})
	} else {
		event("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "tool_use", "id": block["id"], "name": block["name"], "input": map[string]any{}}})
		event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": string(block["input"].(json.RawMessage))}})
	}
	event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	event("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 10}})
	event("message_stop", map[string]any{"type": "message_stop"})
	return err
}
