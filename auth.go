// Copyright 2021 The zombiezen Go Client for Discord Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//		 https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package discord

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	botAuthPrefix    = "Bot "
	bearerAuthPrefix = "Bearer "
)

// AuthHeader is authentication passed as an Authorization HTTP header value.
// https://discord.com/developers/docs/reference#authentication
type AuthHeader string

// BotAuthorization returns the authentication for a bot token.
func BotAuthorization(token string) AuthHeader {
	return AuthHeader(botAuthPrefix + token)
}

// BearerAuthorization returns the authentication for a bearer token.
func BearerAuthorization(token string) AuthHeader {
	return AuthHeader(bearerAuthPrefix + token)
}

// IsValid reports whether the authentication is in a format that Discord will
// accept.
func (auth AuthHeader) IsValid() bool {
	return strings.HasPrefix(string(auth), botAuthPrefix) ||
		strings.HasPrefix(string(auth), bearerAuthPrefix)
}

// token returns the token in the auth header or the empty string if the header
// is invalid.
func (auth AuthHeader) token() string {
	switch {
	case strings.HasPrefix(string(auth), botAuthPrefix):
		return strings.TrimSpace(string(auth[len(botAuthPrefix):]))
	case strings.HasPrefix(string(auth), bearerAuthPrefix):
		return strings.TrimSpace(string(auth[len(bearerAuthPrefix):]))
	default:
		return ""
	}
}

// AuthorizationInfo holds the response to a CurrentAuthorization call.
// https://discord.com/developers/docs/topics/oauth2#get-current-authorization-information-response-structure
type AuthorizationInfo struct {
	Scopes  []string  `json:"scopes"`
	Expires time.Time `json:"expires"`
	User    *User     `json:"user,omitempty"`
}

// CurrentAuthorization returns info about the current authorization.
// https://discord.com/developers/docs/topics/oauth2#get-current-authorization-information
func (c *Client) CurrentAuthorization(ctx context.Context) (*AuthorizationInfo, error) {
	resp, err := c.do(ctx, &apiRequest{
		method: http.MethodGet,
		route:  "/oauth2/@me",
	})
	if err != nil {
		return nil, fmt.Errorf("get current authorization info: %v", err)
	}
	body, err := readBody(resp, maxResponseSize)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("get current authorization info: %v", err)
	}
	parsed := new(AuthorizationInfo)
	if err := unmarshalJSON(body, &parsed); err != nil {
		return nil, fmt.Errorf("get current authorization info: %v", err)
	}
	return parsed, nil
}

type OAuth2Options struct {
	// BaseURL specifies the client's base URL. If nil, the Discord API URL
	// documented in https://discord.com/developers/docs/reference#api-reference-base-url
	// is used.
	BaseURL *url.URL

	// Scopes is the set of scopes to request.
	Scopes []string

	// State is the unique string described in
	// https://discord.com/developers/docs/topics/oauth2#state-and-security
	State string

	Permissions Permissions

	RedirectURI string
}

func OAuth2AuthorizationURL(clientID string, opts *OAuth2Options) *url.URL {
	u := defaultBaseURL()
	if opts != nil && opts.BaseURL != nil {
		*u = *opts.BaseURL
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/oauth2/authorize"
	q := u.Query()
	q.Set("client_id", clientID)
	if opts != nil {
		if len(opts.Scopes) > 0 {
			q.Set("scope", strings.Join(opts.Scopes, " "))
		}
		if opts.State != "" {
			q.Set("state", opts.State)
		}
		if opts.RedirectURI != "" {
			q.Set("redirect_uri", opts.RedirectURI)
		}
		if opts.Permissions != 0 {
			q.Set("permissions", strconv.FormatUint(uint64(opts.Permissions), 10))
		}
	}
	u.RawQuery = q.Encode()
	return u
}
