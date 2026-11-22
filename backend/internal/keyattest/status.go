package keyattest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// GoogleStatusURL is Google's list of revoked and suspended attestation certificates.
const GoogleStatusURL = "https://android.googleapis.com/attestation/status"

// StatusList fetches the revocation list and keeps it for MaxAge (Google serves it with a day's
// max-age). When a refresh fails the previous copy keeps being used; when there has never been one,
// Revoked returns the error and Verify reports the attestation as not revocation-checked.
type StatusList struct {
	URL    string
	Client *http.Client
	MaxAge time.Duration
	Now    func() time.Time

	mu      sync.Mutex
	entries map[string]string // serial → status
	fetched time.Time
	tried   time.Time
}

func (s *StatusList) Revoked(ctx context.Context, serial string) (bool, error) {
	entries, err := s.list(ctx)
	if err != nil {
		return false, err
	}
	st := entries[strings.ToLower(serial)]
	return st == "REVOKED" || st == "SUSPENDED", nil
}

func (s *StatusList) list(ctx context.Context) (map[string]string, error) {
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	maxAge := s.MaxAge
	if maxAge == 0 {
		maxAge = 12 * time.Hour
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// After a failure, wait a minute before trying again, so a Google outage doesn't add a slow
	// request to every sign-in.
	if s.entries != nil && now.Sub(s.fetched) < maxAge || now.Sub(s.tried) < time.Minute {
		if s.entries == nil {
			return nil, fmt.Errorf("attestation status list unavailable")
		}
		return s.entries, nil
	}
	s.tried = now
	entries, err := s.fetch(ctx)
	if err != nil {
		if s.entries != nil {
			return s.entries, nil
		}
		return nil, err
	}
	s.entries, s.fetched = entries, now
	return entries, nil
}

func (s *StatusList) fetch(ctx context.Context) (map[string]string, error) {
	url := s.URL
	if url == "" {
		url = GoogleStatusURL
	}
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("attestation status list: HTTP %d", res.StatusCode)
	}
	var body struct {
		Entries map[string]struct {
			Status string `json:"status"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 20<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("attestation status list: %w", err)
	}
	out := make(map[string]string, len(body.Entries))
	for k, v := range body.Entries {
		out[strings.ToLower(k)] = v.Status
	}
	return out, nil
}
