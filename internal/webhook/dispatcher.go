// Package webhook delivers stream lifecycle events to operator-configured HTTP
// endpoints. Delivery is asynchronous and best-effort so that a slow receiver
// can never block the media path.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// Event is the JSON body POSTed to each configured webhook URL.
type Event struct {
	Type      string         `json:"type"`
	StreamID  string         `json:"streamId"`
	Timestamp time.Time      `json:"timestamp"`
	Data      map[string]any `json:"data,omitempty"`
}

// Event type constants.
const (
	EventStreamPublished = "stream.published"
	EventStreamEnded     = "stream.ended"
	EventStreamCreated   = "stream.created"
	EventStreamDeleted   = "stream.deleted"
)

// Dispatcher fans an Event out to every configured URL.
type Dispatcher struct {
	urls   []string
	secret string
	client *http.Client
	log    *slog.Logger
}

// New returns a Dispatcher. A nil or empty URL list yields a no-op dispatcher.
func New(urls []string, secret string, log *slog.Logger) *Dispatcher {
	if log == nil {
		log = slog.Default()
	}
	return &Dispatcher{
		urls:   urls,
		secret: secret,
		client: &http.Client{Timeout: 5 * time.Second},
		log:    log,
	}
}

// Enabled reports whether any delivery target is configured.
func (d *Dispatcher) Enabled() bool {
	return d != nil && len(d.urls) > 0
}

// Emit delivers the event asynchronously to every target.
func (d *Dispatcher) Emit(event Event) {
	if !d.Enabled() {
		return
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	body, err := json.Marshal(event)
	if err != nil {
		d.log.Error("webhook: marshal", "err", err)
		return
	}
	for _, url := range d.urls {
		go d.post(url, body)
	}
}

func (d *Dispatcher) post(url string, body []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		d.log.Warn("webhook: request", "url", url, "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ari-media-engine")
	if d.secret != "" {
		req.Header.Set("X-Ari-Signature", "sha256="+sign(d.secret, body))
	}

	resp, err := d.client.Do(req)
	if err != nil {
		d.log.Warn("webhook: deliver", "url", url, "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		d.log.Warn("webhook: non-2xx", "url", url, "status", resp.StatusCode)
	}
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
