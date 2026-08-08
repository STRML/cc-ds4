package proxy

import (
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/strml/cc-ds4/src/go/internal/jsonpy"
	"github.com/strml/cc-ds4/src/go/internal/profiles"
	"github.com/strml/cc-ds4/src/go/internal/relay"
)

// Handler is one profile's proxy: an HTTP handler that authenticates local
// clients, rewrites the Claude Code request for the profile's upstream, and
// relays it. client carries the idle-deadline DialContext wrapper (Task 3);
// classifierClient is a SEPARATE deadline-free transport so the RELAY_TIMEOUT
// wrapper never applies to the no-timeout classifier path.
type Handler struct {
	cfg              profiles.Profile
	client           *http.Client // relay client w/ idle timeout + DisableCompression
	classifierClient *http.Client // classifier client: DisableCompression, no deadline wrapper
	br               breaker      // failover circuit breaker (per profile)
}

// NewHandler builds a Handler for one profile. The relay transport sets
// DisableCompression (the proxy negotiates its own identity with a curl
// User-Agent and the upstream's raw encoding is passed through), wraps every
// dial in an idle-deadline conn so a stalled upstream read trips the
// connection, and pools aggressively (256 idle conns per host and total, the
// scale the nightly cc-autodream fanout reaches).
func NewHandler(cfg profiles.Profile, relayTimeout time.Duration) *Handler {
	transport := &http.Transport{
		DisableCompression:  true,
		DialContext:         relay.DialContextWithIdleTimeout(relayTimeout),
		MaxIdleConnsPerHost: 256,
		MaxIdleConns:        256,
	}
	// The classifier rides a separate transport with NO DialContext wrapper:
	// Python's classifier urlopen has no timeout (proxy.py:950,987), and the
	// per-dial idle deadline must not leak onto that no-timeout path. Shared
	// MaxIdle settings mirror the relay transport.
	classifierTransport := &http.Transport{
		DisableCompression:  true,
		MaxIdleConnsPerHost: 256,
		MaxIdleConns:        256,
	}
	return &Handler{
		cfg:              cfg,
		client:           &http.Client{Transport: transport},
		classifierClient: &http.Client{Transport: classifierTransport},
	}
}

// ServeHTTP dispatches the request. Only POST is a relayed request: GET is
// the /__spend endpoint (Task 6), and everything else is 501 (mirroring
// Python's do_GET/do_POST only having two methods). A GET to any other path
// 404s, exactly as Python's do_GET does for a non-/__spend path.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if r.URL.Path == "/__spend" {
			h.spend(w, r)
		} else {
			h.notFound(w)
		}
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(501)
		return
	}
	if !authOK(r, h.cfg) {
		// Match Python's _json(401, ...) body + content-type (proxy.py:757).
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(401)
		io.WriteString(w, `{"error": {"message": "invalid proxy client credential"}}`)
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
	body, requiresZDR, err := requestRequiresZDR(body, r)
	if err != nil {
		http.Error(w, `{"error": {"message": "invalid JSON body"}}`, http.StatusBadRequest)
		return
	}
	if requiresZDR && (!h.cfg.ZDR || os.Getenv("DS4_ZDR") == "0") {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"error": {"message": "request requires ZDR, but this route cannot enforce it"}}`)
		return
	}
	upstreamURL := strings.TrimRight(h.cfg.Upstream, "/") + r.URL.RequestURI()
	h.relay(w, r, body, upstreamURL)
}

func requestRequiresZDR(body []byte, r *http.Request) ([]byte, bool, error) {
	header := strings.ToLower(strings.TrimSpace(r.Header.Get("x-ds4-require-zdr")))
	required := header == "1" || header == "true" || header == "yes"
	clean, err := jsonpy.Marshal(body, func(root *jsonpy.OrderedValue) {
		if v := root.Get("ds4_require_zdr"); v != nil && v.Raw() == "true" {
			required = true
		}
		root.Delete("ds4_require_zdr")
	})
	if err != nil {
		// Python forwards malformed JSON unchanged; only the header signal can
		// still trigger the ZDR gate in that case.
		return body, required, nil
	}
	return clean, required, nil
}

func (h *Handler) notFound(w http.ResponseWriter) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(404)
	io.WriteString(w, `{"error": {"message": "not found"}}`)
}
