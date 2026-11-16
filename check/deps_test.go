package main

import (
	"strings"
	"testing"
)

const goModWithDep = `module example.com/kiln

go 1.27

require example.com/lib v1.2.3

require (
	example.com/other v0.1.0 // indirect
)
`

const complete = `- **For:** x
- **Looked at:** y
- **Why this one:** z
- **Licence:** MIT
`

func doc(sections ...string) string {
	return "# Dependencies\n\n## Go modules\n\n" + strings.Join(sections, "\n") + "\n## Other\n\n### The Go image\n\n- **For:** not read\n"
}

func TestCheckDependencies(t *testing.T) {
	tests := []struct {
		name  string
		goMod string
		doc   string
		want  []string // substrings, one per expected problem
	}{
		{
			name:  "complete entry for the direct dependency",
			goMod: goModWithDep,
			doc:   doc("### example.com/lib\n\n" + complete),
		},
		{
			name:  "no dependencies and no entries",
			goMod: "module example.com/kiln\n\ngo 1.27\n",
			doc:   doc(),
		},
		{
			name:  "a missing entry",
			goMod: goModWithDep,
			doc:   doc(),
			want:  []string{"example.com/lib: direct dependency without an entry"},
		},
		{
			name:  "an incomplete entry",
			goMod: goModWithDep,
			doc:   doc("### example.com/lib\n\n- **For:** x\n- **Looked at:** y\n- **Licence:**\n"),
			want:  []string{`lacks "Why this one"`, `lacks "Licence"`},
		},
		{
			name:  "an entry without a dependency",
			goMod: "module example.com/kiln\n\ngo 1.27\n",
			doc:   doc("### example.com/gone\n\n" + complete),
			want:  []string{"example.com/gone: entry without a direct dependency, and not marked as replaced"},
		},
		{
			name:  "an entry marked as replaced",
			goMod: "module example.com/kiln\n\ngo 1.27\n",
			doc:   doc("### example.com/gone\n\n" + complete + "- **Replaced:** by example.com/lib, which is smaller\n"),
		},
		{
			name:  "a replaced entry for a dependency still required",
			goMod: goModWithDep,
			doc:   doc("### example.com/lib\n\n" + complete + "- **Replaced:** by something\n"),
			want:  []string{"marked as replaced, but it is still a direct dependency"},
		},
		{
			name:  "an indirect requirement needs no entry",
			goMod: "module example.com/kiln\n\nrequire example.com/lib v1.0.0 // indirect\n",
			doc:   doc(),
		},
		{
			name:  "text on a following line counts",
			goMod: goModWithDep,
			doc:   doc("### example.com/lib\n\n- **For:** x\n- **Looked at:**\n  y\n- **Why this one:** z\n- **Licence:** MIT\n"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkDependencies(tt.goMod, tt.doc)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d problems %q, want %d %q", len(got), got, len(tt.want), tt.want)
			}
			for i, w := range tt.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("problem %d is %q, want it to contain %q", i, got[i], w)
				}
			}
		})
	}
}
