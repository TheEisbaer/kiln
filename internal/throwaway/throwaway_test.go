package throwaway

import "testing"

// Fails on purpose, to show that a failing check keeps a pull request from merging.
func TestFailsOnPurpose(t *testing.T) {
	t.Fatal("this test fails on purpose")
}
