package proxy

import (
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/strml/cc-ds4/src/go/internal/profiles"
	"github.com/strml/cc-ds4/src/go/internal/relay"
)

// Handler is one profile's proxy: an HTTP handler that authenticates local
// clients, rewrites the Claude Code request for the profile's upstream, and
// relays it. The classifier client (a separate, no-timeout transport) is
// added in Task 7.
type Handler struct {
	cfg    profiles.Profile
	client *http.Client // relay client w/ idle timeout + DisableCompression
}

// NewHandler builds a Handler for one profile. The relay transport sets
// DisableCompression (the proxy negotiates its own identity with a curl
// User-Agent and the upstream's raw encoding is passed through), wraps every
// dial in an idle-deadline conn so a stalled upstream read trips the
// connection, and pools aggressively (256 idle conns per host and total, the
// scale the nightly cc-autodream fanout reaches).
func NewHandler(cfg profiles.Profile, relayTimeout time.Duration) http.Handler {
	transport := &http.Transport{
		DisableCompression:  true,
		DialContext:         relay.DialContextWithIdleTimeout(relayTimeout),
		MaxIdleConnsPerHost: 256,
		MaxIdleConns:        256,
	}
	return &Handler{cfg: cfg, client: &http.Client{Transport: transport}}
}

// ServeHTTP dispatches the request. Only POST is a relayed request: GET is
// the spend endpoint (Task 6), and everything else is 501 (mirroring Python's
// do_GET/do_POST only having two methods).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		h.spend(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(501)
		return
	}
	if !authOK(r, h.cfg) {
		w.WriteHeader(401)
		return
	}
	// A malformed or absent Content-Length must not panic or reach upstream;
	// http.NewRequest would reject a negative value, and an empty body is a
	// valid request the upstream can answer.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error": {"message": "cannot read request body"}}`, 400)
		return
	}
	upstreamURL := strings.TrimRight(h.cfg.Upstream, "/") + r.URL.RequestURI()
	h.relay(w, r, body, upstreamURL)
}

func (h *Handler) notFound(w http.ResponseWriter) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(404)
	io.WriteString(w, `{"error": {"message": "not found"}}`)
}
