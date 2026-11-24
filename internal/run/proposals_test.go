package run

import (
	"reflect"
	"testing"
)

// TestProposals reads proposals.md in the shapes a review run writes, and
// those it writes slightly wrong.
func TestProposals(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []Proposal
	}{
		{"two", "Proposal: The first\nfoo.go:12 panics on nil.\n\nProposal: The second\nbar.go:3 leaks a file.\nIt is never closed.\n",
			[]Proposal{{"The first", "foo.go:12 panics on nil."}, {"The second", "bar.go:3 leaks a file.\nIt is never closed."}}},
		{"none", "", nil},
		{"blank", " \n\n\t\n", nil},
		{"marks", "**Proposal 1:** One\nIts text.\n## Proposal: Two ##\nIts text.\n- proposal: Three\nIts text.\n**Proposal 2**: Four\n",
			[]Proposal{{"One", "Its text."}, {"Two", "Its text."}, {"Three", "Its text."}, {"Four", ""}}},
		{"proposals is no title", "Proposal: One\nProposals: two of them.\n",
			[]Proposal{{"One", "Proposals: two of them."}}},
		{"a heading on top", "# Proposals\n\nProposal: One\nA.\n\nProposal: Two\nB.\n",
			[]Proposal{{"One", "A."}, {"Two", "B."}}},
		{"prose before the first", "# Proposals\n\nI noticed this  \nin passing.\n\nProposal: One\nA.\nProposal: Two\nB.\n",
			[]Proposal{{"", "# Proposals\n\nI noticed this  \nin passing."}, {"One", "A."}, {"Two", "B."}}},
		{"no title line", "\nSomething is wrong in foo.go:12\n  it panics on nil\n\n",
			[]Proposal{{"", "Something is wrong in foo.go:12\n  it panics on nil"}}},
		{"an empty title", "Proposal:\nThe text without a title.\n", []Proposal{{"", "The text without a title."}}},
		{"an empty title and no text", "Proposal:   \n\nProposal: One\nA.\n", []Proposal{{"One", "A."}}},
		{"none said", "Proposal: none\n", nil},
		{"None. said", "# Proposals\n\n- **Proposal: None.**\n", nil},
		{"none with a text", "Proposal: None\nBut here is one.\n", []Proposal{{"None", "But here is one."}}},
		{"CRLF", "Proposal: One\r\nA.\r\n\r\nProposal: Two\r\nB.\r\n",
			[]Proposal{{"One", "A.\r"}, {"Two", "B.\r"}}},
	} {
		if got := Proposals([]byte(tc.in)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: Proposals(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}
