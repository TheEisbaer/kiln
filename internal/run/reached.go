package run

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Reached is what a run's way out carried to one host, read from
// <runs>/<id>/wayout.jsonl (ADR 0059).
type Reached struct {
	Host string
	// Connections are the requests let through with 200, each one connection;
	// Ended are the connections whose end the way out wrote. A connection open
	// when the way out was killed, or carried by a way out from before the end
	// lines, has no end and is not counted in Sent and Received.
	Connections, Ended int
	// The bytes from the run to the host and from the host to the run, over
	// the ended connections.
	Sent, Received int64
}

// ReadReached reads the hosts run id reached from <runs>/<id>/wayout.jsonl,
// the host that sent most first, then by name. It returns nil when there is no
// such file: a run without a list, one whose way out was gone, or no record.
// A line that is not JSON, such as a last line cut short by a kill, is
// skipped. It writes nothing.
func ReadReached(runs, id string) ([]Reached, error) {
	f, err := os.Open(filepath.Join(runs, id, "wayout.jsonl"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	byHost := map[string]*Reached{}
	of := func(target string) *Reached {
		h := reachedHost(target)
		if byHost[h] == nil {
			byHost[h] = &Reached{Host: h}
		}
		return byHost[h]
	}
	// A refused target may be as long as a request's header, so a line has
	// no limit of length.
	r := bufio.NewReader(f)
	for {
		line, readErr := r.ReadBytes('\n')
		var e struct {
			Target   string `json:"target"`
			Decision string `json:"decision"`
			Status   int    `json:"status"`
			End      string `json:"end"`
			Sent     int64  `json:"sent"`
			Received int64  `json:"received"`
		}
		if len(line) > 0 && json.Unmarshal(line, &e) == nil {
			switch {
			case e.End != "":
				h := of(e.Target)
				h.Ended++
				h.Sent += e.Sent
				h.Received += e.Received
			case e.Decision == "allowed" && e.Status == 200:
				of(e.Target).Connections++
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	out := make([]Reached, 0, len(byHost))
	for _, h := range byHost {
		out = append(out, *h)
	}
	slices.SortFunc(out, func(a, b Reached) int {
		if c := cmp.Compare(b.Sent, a.Sent); c != 0 {
			return c
		}
		return cmp.Compare(a.Host, b.Host)
	})
	return out, nil
}

// reachedHost is a target's host as the way out reads it: without the port
// 443, in lower case, without one dot at its end.
func reachedHost(target string) string {
	h := strings.TrimSuffix(target, ":443")
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	return h
}
