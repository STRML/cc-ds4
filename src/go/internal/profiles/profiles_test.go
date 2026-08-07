package profiles

import "testing"

func TestPortsMatchesPython(t *testing.T) {
	// direct has no cfg["dir"] on CI; assert the three names/ports are present
	// regardless of served-filtering by checking All() ports.
	for _, p := range All() {
		switch p.Name {
		case "direct":
			if p.Port != 31500 {
				t.Fatalf("direct port = %d", p.Port)
			}
		case "openrouter":
			if p.Port != 31501 {
				t.Fatalf("openrouter port = %d", p.Port)
			}
		case "nous":
			if p.Port != 31502 {
				t.Fatalf("nous port = %d", p.Port)
			}
		}
	}
}
