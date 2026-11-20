package run

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// event is what is read of one line of Claude Code's stream-json output
// (version 2.1.326, pinned in images/run/Dockerfile).
type event struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	// system/init
	Model   string `json:"model"`
	Version string `json:"claude_code_version"`

	// assistant
	Message struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`

	// result
	IsError      bool    `json:"is_error"`
	NumTurns     int     `json:"num_turns"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

// Result is what the record keeps of the last result event.
type Result struct {
	Subtype      string  `json:"subtype"`
	IsError      bool    `json:"is_error"`
	NumTurns     int     `json:"num_turns"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

// summary is what a run's output says: the model system/init names, the last
// result event, if any, and how many lines were passed over as too long.
type summary struct {
	model   string
	result  *Result
	tooLong int
}

// maxLine is the longest line of a run's output that is read as an event; a
// longer one is passed over and counted, so that one huge line without a
// newline cannot fill the developer's machine's memory. The longest line of the
// scripted run's output.jsonl is 2,751 bytes (measured on 2026-11-17 with
// Claude Code 2.1.326). A real event carries a tool's input or result, such as
// a file that was read or a command's output, and can be far larger; no real
// run was measured, so the limit is generous: 64 MiB, over 20,000 times the
// measured line. The recorded output.jsonl is written as a stream and keeps
// every line, whatever its length.
const maxLine = 64 << 20

// readEvents reads a run's output line by line, each up to maxLine. For every
// event it calls see, which may be nil, and it returns the summary. A line
// that is not an event is passed over.
func readEvents(r io.Reader, see func(event)) (summary, error) {
	return readEventsUpTo(r, see, maxLine)
}

func readEventsUpTo(r io.Reader, see func(event), limit int) (summary, error) {
	var s summary
	br := bufio.NewReader(r)
	var line []byte
	tooLong := false // the line so far is longer than limit and is not kept
	for {
		chunk, err := br.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > limit {
				tooLong, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue // the same line goes on
		}
		if tooLong {
			s.tooLong++
			tooLong = false
		} else if len(line) > 0 {
			var e event
			if json.Unmarshal(line, &e) == nil {
				switch {
				case e.Type == "system" && e.Subtype == "init" && s.model == "":
					s.model = e.Model
				case e.Type == "result":
					s.result = &Result{Subtype: e.Subtype, IsError: e.IsError, NumTurns: e.NumTurns, TotalCostUSD: e.TotalCostUSD}
				}
				if see != nil {
					see(e)
				}
			}
		}
		line = line[:0]
		if err == io.EOF {
			return s, nil
		}
		if err != nil {
			return s, err
		}
	}
}

// describe returns the lines that following prints for an event: the model
// and version from system/init, a tool's name and description, the first line
// of a text, and the result with its subtype, turns and cost.
func describe(e event) []string {
	switch {
	case e.Type == "system" && e.Subtype == "init":
		return []string{fmt.Sprintf("model %s, Claude Code %s", e.Model, e.Version)}
	case e.Type == "assistant":
		var lines []string
		for _, c := range e.Message.Content {
			switch c.Type {
			case "tool_use":
				if d := inputField(c.Input, "description"); d != "" {
					lines = append(lines, fmt.Sprintf("tool %s: %s", c.Name, firstLine(d)))
				} else {
					lines = append(lines, "tool "+c.Name)
				}
			case "text":
				if t := firstLine(c.Text); t != "" {
					lines = append(lines, "text: "+t)
				}
			}
		}
		return lines
	case e.Type == "result":
		return []string{fmt.Sprintf("result %s, %d turns, $%.4f", e.Subtype, e.NumTurns, e.TotalCostUSD)}
	}
	return nil
}

// inputField is the text of a field of a tool's input, or "" when the input
// has none or it is not text.
func inputField(input json.RawMessage, field string) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(input, &fields) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(fields[field], &s) != nil {
		return ""
	}
	return s
}

// Activity is one thing a run did, as the screen shows it while the run is
// followed: a text the agent wrote, or a tool it used, with when it was read.
type Activity struct {
	At   time.Time
	Kind string // "text" or "tool"
	Text string // the text, or one line of the tool's input
	Tool string // the tool's name
}

// The longest text of an activity, in characters, and the longest line of a
// tool's input.
const (
	maxActivityText = 500
	maxActivityLine = 200
)

// inputFields are the fields of a tool's input that say best what it does,
// in the order they are looked for.
var inputFields = []string{"description", "command", "file_path", "path", "url", "pattern", "query", "prompt"}

// activities are the activities of an event: each text the agent wrote, cut
// at 500 characters, and each tool use, with its name and one line of its
// input: the first of inputFields it has, or else the input itself.
func activities(e event, at time.Time) []Activity {
	if e.Type != "assistant" {
		return nil
	}
	var out []Activity
	for _, c := range e.Message.Content {
		switch c.Type {
		case "text":
			if t := strings.TrimSpace(c.Text); t != "" {
				out = append(out, Activity{At: at, Kind: "text", Text: cut(t, maxActivityText)})
			}
		case "tool_use":
			line := ""
			for _, f := range inputFields {
				if line = firstLine(inputField(c.Input, f)); line != "" {
					break
				}
			}
			if line == "" && len(c.Input) > 0 && string(c.Input) != "{}" {
				line = firstLine(string(c.Input))
			}
			out = append(out, Activity{At: at, Kind: "tool", Tool: c.Name, Text: cut(line, maxActivityLine)})
		}
	}
	return out
}

// cut is s cut at n characters, with an ellipsis when it was cut.
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// firstLine is the first line of a text that is not empty.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// follower prints the events of a run as they come, hands their activities to
// activity, when it is set, and calls mismatch once when system/init names
// another model than the one asked for.
type follower struct {
	out      io.Writer
	model    string
	mismatch func(seen string)
	activity func(Activity)
	stopped  bool
}

func (f *follower) see(e event) {
	for _, line := range describe(e) {
		fmt.Fprintln(f.out, printable(line))
	}
	if f.activity != nil {
		for _, a := range activities(e, time.Now().UTC()) {
			f.activity(a)
		}
	}
	if e.Type == "system" && e.Subtype == "init" && e.Model != f.model && !f.stopped {
		f.stopped = true
		f.mismatch(e.Model)
	}
}
