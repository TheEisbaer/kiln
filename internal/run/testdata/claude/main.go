// Command claude stands in for Claude in the run test. It answers Claude Code's
// Messages requests from a script: a Bash call `sh run.sh` first, then the text
// "Done." once a tool result has come. Its data follows the Claude Code version
// pinned in images/run/Dockerfile; the card that changes the pin updates it.
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
}

func main() {
	if len(os.Args) != 2 || os.Args[1] == "" {
		log.Fatal("usage: claude <marker>")
	}
	marker := os.Args[1]
	out := json.NewEncoder(os.Stdout)
	log.Fatal(http.ListenAndServe(port, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req request
		_ = json.Unmarshal(body, &req)
		toolResult := hasToolResult(req)
		start := time.Now()
		caseName := caseWord.FindString(string(body))
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
		})

		w.Header().Set("Content-Type", "application/json")
		var err error
		switch {
		case r.Method != http.MethodPost:
			_, err = w.Write([]byte("{}"))
		case strings.HasSuffix(r.URL.Path, "/count_tokens"):
			_, err = w.Write([]byte(`{"input_tokens":10}`))
		case r.URL.Path == "/v1/messages":
			err = answer(w, req, toolResult)
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

// hasToolResult reports whether a request carries a tool result.
func hasToolResult(req request) bool {
	for _, m := range req.Messages {
		var blocks []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue // a plain string
		}
		for _, b := range blocks {
			if b.Type == "tool_result" {
				return true
			}
		}
	}
	return false
}

// The scripted answers. The text carries an escape sequence, which the test
// expects in the record and not on the terminal.
const (
	bashInput = `{"command":"sh run.sh","description":"Run the script"}`
	doneText  = "Done.\x1b[2K"
)

// answer writes the next answer of the script, streamed or whole, and returns
// the first error of writing it.
func answer(w http.ResponseWriter, req request, toolResult bool) error {
	usage := map[string]int{"input_tokens": 100, "output_tokens": 10,
		"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}
	var block map[string]any
	stop := "tool_use"
	if toolResult {
		block = map[string]any{"type": "text", "text": doneText}
		stop = "end_turn"
	} else {
		block = map[string]any{"type": "tool_use", "id": "toolu_kiln_1", "name": "Bash", "input": json.RawMessage(bashInput)}
	}
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
	if toolResult {
		event("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""}})
		event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": doneText}})
	} else {
		event("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "tool_use", "id": "toolu_kiln_1", "name": "Bash", "input": map[string]any{}}})
		event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": bashInput}})
	}
	event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	event("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 10}})
	event("message_stop", map[string]any{"type": "message_stop"})
	return err
}
