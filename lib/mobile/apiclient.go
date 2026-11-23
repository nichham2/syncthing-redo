// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package mobile

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/syncthing/syncthing/lib/protocol"
)

// apiClient talks to the node's own REST API over loopback. Using the
// public API (rather than reaching into the model) keeps the bridge
// decoupled from core internals.
type apiClient struct {
	base   string
	apiKey string
	myID   protocol.DeviceID
	http   *http.Client
}

func newAPIClient(base, apiKey string, myID protocol.DeviceID) *apiClient {
	return &apiClient{
		base:   strings.TrimSuffix(base, "/"),
		apiKey: apiKey,
		myID:   myID,
		// Scans of large files can take a while; the timeout only guards
		// against a hung server.
		http: &http.Client{Timeout: 2 * time.Minute},
	}
}

func (c *apiClient) do(ctx context.Context, method, path string, query map[string]string, body, out any) error {
	u := c.base + path
	if len(query) > 0 {
		q := url.Values{}
		for k, v := range query {
			q.Set(k, v)
		}
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		bs, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(bs)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Only talk to our own node. Another process listening on the port
	// would not answer with our device ID.
	if got := resp.Header.Get("X-Syncthing-Id"); got != c.myID.String() {
		return fmt.Errorf("%s %s: unexpected server %q on %s", method, path, got, c.base)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *apiClient) get(ctx context.Context, path string, query map[string]string, out any) error {
	return c.do(ctx, http.MethodGet, path, query, nil, out)
}

func (c *apiClient) post(ctx context.Context, path string, query map[string]string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, query, body, out)
}
