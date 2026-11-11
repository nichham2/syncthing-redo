// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package mobile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// apiClient talks to the node's own REST API over loopback. Using the
// public API (rather than reaching into the model) keeps the bridge
// decoupled from core internals.
type apiClient struct {
	base   string
	apiKey string
	http   *http.Client
}

func newAPIClient(base, apiKey string) *apiClient {
	return &apiClient{
		base:   strings.TrimSuffix(base, "/"),
		apiKey: apiKey,
		http:   &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *apiClient) do(method, path string, query map[string]string, body, out any) error {
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
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *apiClient) get(path string, query map[string]string, out any) error {
	return c.do(http.MethodGet, path, query, nil, out)
}

func (c *apiClient) post(path string, query map[string]string, body, out any) error {
	return c.do(http.MethodPost, path, query, body, out)
}
