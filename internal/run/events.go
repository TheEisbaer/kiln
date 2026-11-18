package run

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
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
			Type  string `json:"type"`
			Text  string `json:"text"`
			Name  string `json:"name"`
			Input struct {
				Description string `json:"description"`
			} `json:"input"`
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
				if c.Input.Description != "" {
					lines = append(lines, fmt.Sprintf("tool %s: %s", c.Name, firstLine(c.Input.Description)))
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

// firstLine is the first line of a text that is not empty.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// follower prints the events of a run as they come, and calls mismatch once
// when system/init names another model than the one asked for.
type follower struct {
	out      io.Writer
	model    string
	mismatch func(seen string)
	stopped  bool
}

func (f *follower) see(e event) {
	for _, line := range describe(e) {
		fmt.Fprintln(f.out, printable(line))
	}
	if e.Type == "system" && e.Subtype == "init" && e.Model != f.model && !f.stopped {
		f.stopped = true
		f.mismatch(e.Model)
	}
}
