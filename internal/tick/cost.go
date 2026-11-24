package tick

import (
	"fmt"
	"io"

	"github.com/TheEisbaer/kiln/internal/run"
	"github.com/TheEisbaer/kiln/internal/store"
)

// fillCosts reads the cost of the runs that ended before the store kept it,
// once, from <runs>/<id>/result.json (ADR 0029, ADR 0035). A run without a
// record, or with one that cannot be read, keeps an unknown cost and is read
// no more. A failure is printed and the core goes on; the runs are read at the
// next start.
func fillCosts(s *store.Store, runs string, out io.Writer) {
	ids, err := s.UnreadCosts()
	if err != nil {
		fmt.Fprintln(out, "kiln core: the cost of earlier runs is not read:", run.Printable(err.Error()))
		return
	}
	if len(ids) == 0 {
		return
	}
	costs := make(map[string]store.Cost, len(ids))
	unknown := 0
	for _, id := range ids {
		usd, session, err := run.ReadCost(runs, id)
		if err != nil {
			fmt.Fprintln(out, "kiln core: the cost of run", id, "is not read:", run.Printable(err.Error()))
		}
		if usd == nil {
			unknown++
		}
		costs[id] = store.Cost{USD: usd, Session: session}
	}
	if err := s.FillCosts(costs); err != nil {
		fmt.Fprintln(out, "kiln core: the cost of earlier runs is not read:", run.Printable(err.Error()))
		return
	}
	fmt.Fprintf(out, "kiln core: read the cost of %d earlier runs from their records; %d have no cost\n", len(ids), unknown)
}
