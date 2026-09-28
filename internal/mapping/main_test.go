package mapping

import (
	"os"
	"testing"
)

// TestMain opts in to plain-HTTP loopback mapping URLs so httptest servers
// can stand in for the upstream.
func TestMain(m *testing.M) {
	if err := os.Setenv("ALLOW_INSECURE_MAPPING_URL", "1"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
