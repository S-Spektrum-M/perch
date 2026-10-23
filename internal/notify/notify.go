package notify

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Notifier publishes minimal session metadata, never transcript or dialog content.
type Notifier struct {
	topic   string
	token   string
	baseURL string
	client  *http.Client
}

func New(topic, token, baseURL string) (*Notifier, error) {
	if topic == "" {
		return nil, nil
	}
	for _, raw := range []string{topic, baseURL} {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
			return nil, fmt.Errorf("notification URLs must be absolute HTTP(S) URLs without credentials, query, or fragment")
		}
	}
	return &Notifier{topic: topic, token: token, baseURL: strings.TrimRight(baseURL, "/"), client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (n *Notifier) Send(id, label, backend string) {
	name := backendName(backend)
	n.send(id, name+" is done", name+" finished working in "+label+".")
}

// Question sends an attention notification without disclosing dialog content.
func (n *Notifier) Question(id, label, backend string) {
	name := backendName(backend)
	n.send(id, name+" needs your input", name+" is waiting for your answer in "+label+".")
}

func backendName(backend string) string {
	if backend == "claude" {
		return "Claude"
	}
	return "Pi"
}

func (n *Notifier) send(id, title, body string) {
	req, err := http.NewRequest(http.MethodPost, n.topic, strings.NewReader(body))
	if err != nil {
		log.Printf("notification: could not create request")
		return
	}
	req.Header.Set("Title", title)
	if n.token != "" {
		req.Header.Set("Authorization", "Bearer "+n.token)
	}
	if n.baseURL != "" {
		req.Header.Set("Click", n.baseURL+"/s/"+url.PathEscape(id))
		req.Header.Set("Icon", n.baseURL+"/static/icon-192.png")
	}
	resp, err := n.client.Do(req)
	if err != nil {
		log.Printf("notification: delivery failed (check ntfy connectivity)")
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("notification: ntfy returned HTTP %d", resp.StatusCode)
	}
}
