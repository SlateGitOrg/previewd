package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-logr/logr"

	"github.com/archit2501/previewd/internal/intake"
	"github.com/archit2501/previewd/internal/metrics"
)

// DefaultMaxBodyBytes bounds request bodies. GitHub caps payloads at 25 MB,
// but pull_request events are a few tens of KB; anything near the cap is not
// a pull_request event previewd wants.
const DefaultMaxBodyBytes = 1 << 20

// Applier receives verified, parsed events. *intake.Sink implements it.
type Applier interface {
	Apply(ctx context.Context, ev intake.Event) (intake.Result, error)
}

// Handler is the HTTP endpoint GitHub delivers webhooks to.
type Handler struct {
	// Secrets are the accepted HMAC secrets (several during rotation).
	Secrets [][]byte
	// Sink applies parsed events.
	Sink Applier
	// Deliveries, if set, short-circuits redelivered delivery IDs.
	Deliveries *DeliveryCache
	// MaxBodyBytes bounds the request body (default DefaultMaxBodyBytes).
	MaxBodyBytes int64
	// AllowForks enables previews for pull requests from forks.
	AllowForks bool
	Log        logr.Logger
}

// Response is the JSON body returned to the sender, useful when reading
// GitHub's "Recent Deliveries" page.
type Response struct {
	Status   string `json:"status"`
	Instance string `json:"instance,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

func (h *Handler) maxBody() int64 {
	if h.MaxBodyBytes > 0 {
		return h.MaxBodyBytes
	}
	return DefaultMaxBodyBytes
}

// ServeHTTP verifies, parses and applies one delivery.
//
// Status codes: 405 wrong method, 413 body too large, 401 bad signature,
// 400 malformed, 200 ping/duplicate, 202 accepted or ignored, 503 retry
// later, 500 internal error. The signature is checked before anything in the
// body is parsed.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	event := r.Header.Get(EventHeader)
	delivery := r.Header.Get(DeliveryHeader)
	log := h.Log.WithValues("event", event, "delivery", delivery)
	metricEvent := event
	if metricEvent == "" {
		metricEvent = "unknown"
	} else if metricEvent != "pull_request" && metricEvent != "ping" {
		metricEvent = "other"
	}
	reply := func(code int, outcome string, resp Response) {
		metrics.WebhookRequests.WithLabelValues(metricEvent, outcome).Inc()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(resp)
	}

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		reply(http.StatusMethodNotAllowed, "bad_request", Response{Status: "method not allowed"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody()))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			reply(http.StatusRequestEntityTooLarge, "bad_request", Response{Status: "payload too large"})
			return
		}
		reply(http.StatusBadRequest, "bad_request", Response{Status: "unreadable body"})
		return
	}
	if err := Verify(h.Secrets, body, r.Header.Get(SignatureHeader)); err != nil {
		log.Info("rejected delivery", "reason", err.Error(), "remote", r.RemoteAddr)
		reply(http.StatusUnauthorized, "unauthorized", Response{Status: "invalid signature"})
		return
	}
	switch event {
	case "ping":
		reply(http.StatusOK, "pong", Response{Status: "pong"})
		return
	case "pull_request":
	case "":
		reply(http.StatusBadRequest, "bad_request", Response{Status: "missing " + EventHeader + " header"})
		return
	default:
		reply(http.StatusAccepted, "ignored", Response{Status: "ignored", Reason: "event " + event + " is not handled"})
		return
	}
	if h.Deliveries != nil && h.Deliveries.Seen(delivery) {
		reply(http.StatusOK, "duplicate", Response{Status: "duplicate"})
		return
	}
	ev, err := ParsePullRequest(body, delivery, h.AllowForks)
	switch {
	case IsIrrelevant(err):
		h.record(delivery)
		reply(http.StatusAccepted, "ignored", Response{Status: "ignored", Reason: err.Error()})
		return
	case err != nil:
		log.Info("malformed delivery", "error", err.Error())
		reply(http.StatusBadRequest, "bad_request", Response{Status: "bad request", Reason: err.Error()})
		return
	}
	res, err := h.Sink.Apply(r.Context(), ev)
	switch {
	case errors.Is(err, intake.ErrDeleting):
		reply(http.StatusServiceUnavailable, "error", Response{Status: "retry later", Reason: err.Error()})
		return
	case err != nil:
		log.Error(err, "applying delivery")
		reply(http.StatusInternalServerError, "error", Response{Status: "error"})
		return
	}
	h.record(delivery)
	reply(http.StatusAccepted, string(res.Outcome), Response{Status: string(res.Outcome), Instance: res.Instance, Reason: res.Reason})
}

func (h *Handler) record(delivery string) {
	if h.Deliveries != nil {
		h.Deliveries.Record(delivery)
	}
}
