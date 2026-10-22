package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Notifier publishes minimal completion metadata, never transcript content.
type Notifier struct {
	topic   string
	token   string
	baseURL string
	client  *http.Client
}

func newNotifier(topic, token, baseURL string) (*Notifier, error) {
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

func (n *Notifier) send(id, label string) {
	req, err := http.NewRequest(http.MethodPost, n.topic, strings.NewReader("Pi finished working in "+label+"."))
	if err != nil {
		log.Printf("notification: could not create request")
		return
	}
	req.Header.Set("Title", "Pi is done")
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
