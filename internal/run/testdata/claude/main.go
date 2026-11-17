// Command claude stands in for Claude in the run test. It answers Claude Code's
// Messages requests from a script: a Bash call `sh run.sh` first, then the text
// "Done." once a tool result has come. Its data follows the Claude Code version
// pinned in images/run/Dockerfile; the card that changes the pin updates it.
//
// For each request it writes one line of JSON to standard output: the model,
// output_config.effort, the x-api-key header, whether the marker given as its
// argument is in the body, and the case named in the body, a word
// kiln-case-<name>, by which the test tells its runs apart.
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
)

// port is where it listens, inside its container.
const port = ":8081"

// caseWord names a test case in an instruction.
var caseWord = regexp.MustCompile(`kiln-case-[a-z0-9-]+`)

// request is what is read of a Messages request.
type request struct {
	Model        string `json:"model"`
	Stream       bool   `json:"stream"`
	OutputConfig struct {
		Effort string `json:"effort"`
	} `json:"output_config"`
	Messages []struct {
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// logLine is what is written per request.
type logLine struct {
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
		out.Encode(logLine{
			Method:     r.Method,
			Path:       r.URL.Path,
			Model:      req.Model,
			Effort:     req.OutputConfig.Effort,
			APIKey:     r.Header.Get("X-Api-Key"),
			Marker:     strings.Contains(string(body), marker),
			Case:       caseWord.FindString(string(body)),
			ToolResult: toolResult,
		})

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method != http.MethodPost:
			w.Write([]byte("{}"))
		case strings.HasSuffix(r.URL.Path, "/count_tokens"):
			w.Write([]byte(`{"input_tokens":10}`))
		case r.URL.Path == "/v1/messages":
			answer(w, req, toolResult)
		default:
			http.NotFound(w, r)
		}
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

// The scripted answers.
const (
	bashInput = `{"command":"sh run.sh","description":"Run the script"}`
	doneText  = "Done."
)

// answer writes the next answer of the script, streamed or whole.
func answer(w http.ResponseWriter, req request, toolResult bool) {
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
		json.NewEncoder(w).Encode(message)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	event := func(name string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
		w.(http.Flusher).Flush()
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
}
