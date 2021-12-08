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

// Package discord provides a Discord API client.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/semconv/v1.7.0"
	"go.opentelemetry.io/otel/trace"
)

const maxResponseSize = 4 << 20 // 4 MiB

// HTTP headers
const (
	authorizationHeaderName = "Authorization"
	contentTypeHeaderName   = "Content-Type"
	retryAfterHeaderName    = "Retry-After"
	userAgentHeaderName     = "User-Agent"
)

const (
	jsonMediaType           = "application/json"
	outgoingJSONContentType = "application/json; charset=utf-8"
)

func tracer() trace.Tracer {
	return otel.Tracer("zombiezen.com/go/discord")
}

// Client is an authenticated client of the Discord API.
type Client struct {
	base      url.URL
	auth      AuthHeader
	http      *http.Client
	userAgent string

	mu                   sync.Mutex
	rateLimitBuckets     map[string]string // route -> bucket
	rateLimits           map[rateLimitKey]*rateLimit
	globalRateLimitReset time.Time
}

// ClientOptions holds optional parameters for NewClient.
type ClientOptions struct {
	// BaseURL specifies the client's base URL. If nil, the Discord API URL
	// documented in https://discord.com/developers/docs/reference#api-reference-base-url
	// is used.
	BaseURL *url.URL

	// HTTPClient is used to make HTTP requests. If nil, http.DefaultClient is used.
	HTTPClient *http.Client

	// UserAgent is the name of the application accessing Discord.
	UserAgent string
}

// NewClient returns a new client. opts may be nil. NewClient panics if
// auth.IsValid() reports false.
func NewClient(auth AuthHeader, opts *ClientOptions) *Client {
	if !auth.IsValid() {
		panic("invalid Authorization passed to discord.NewClient")
	}
	c := &Client{
		auth:      auth,
		base:      *defaultBaseURL(),
		http:      http.DefaultClient,
		userAgent: "zombiezen.com/go/discord " + moduleVersion(),

		rateLimitBuckets: make(map[string]string),
		rateLimits:       make(map[rateLimitKey]*rateLimit),
	}
	if opts != nil {
		if opts.BaseURL != nil {
			c.base = *opts.BaseURL
		}
		if opts.HTTPClient != nil {
			c.http = opts.HTTPClient
		}
		if opts.UserAgent != "" {
			c.userAgent = opts.UserAgent
		}
	}
	return c
}

type apiRequest struct {
	method   string
	route    string
	pathVars map[string]string
	jsonData []byte
}

func (req *apiRequest) resolveURL(base *url.URL) *url.URL {
	u := new(url.URL)
	*u = *base
	path := req.route
	if len(req.pathVars) > 0 {
		replacements := make([]string, 0, len(req.pathVars)*2)
		for k, v := range req.pathVars {
			replacements = append(replacements, "{"+k+"}", v)
		}
		path = strings.NewReplacer(replacements...).Replace(path)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.TrimLeft(path, "/")
	return u
}

func (req *apiRequest) newHTTP(base *url.URL, userAgent string) *http.Request {
	h := &http.Request{
		Method: req.method,
		URL:    req.resolveURL(base),
		Header: http.Header{
			userAgentHeaderName: {userAgent},
		},
	}
	if len(req.jsonData) > 0 {
		h.Header.Set(contentTypeHeaderName, outgoingJSONContentType)
		h.Body = io.NopCloser(bytes.NewReader(req.jsonData))
		h.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(req.jsonData)), nil
		}
		h.ContentLength = int64(len(req.jsonData))
	}
	return h
}

func (c *Client) do(ctx context.Context, req *apiRequest) (_ *http.Response, err error) {
	errURL := req.resolveURL(&c.base)
	errURL.User = nil
	errURL.Fragment = ""
	errURL.RawFragment = ""
	errURL.RawQuery = ""
	errURL.ForceQuery = false
	ctx, span := tracer().Start(
		ctx,
		fmt.Sprintf("Discord %s %s", req.method, req.route),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(semconv.HTTPClientAttributesFromHTTPRequest(req.newHTTP(&c.base, c.userAgent))...),
	)
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "")
		}
		span.End()
	}()

	var resp *http.Response
	for {
		// Wait for rate limit, if known.
		if err := c.waitForRateLimit(ctx, req); err != nil {
			return nil, fmt.Errorf("%s: %w", errURL, err)
		}

		// Make request.
		var err error
		httpReq := req.newHTTP(&c.base, c.userAgent).WithContext(ctx)
		httpReq.Header[authorizationHeaderName] = []string{string(c.auth)}
		resp, err = c.http.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", errURL, err)
		}

		// Update rate limits from headers.
		bucket := resp.Header.Get(rateLimitBucketHeader)
		if bucket != "" {
			c.mu.Lock()
			c.rateLimitBuckets[req.rateLimitRoute()] = bucket
			k := rateLimitKey{
				bucket:     bucket,
				majorParam: req.rateLimitMajorParam(),
			}
			limit := c.rateLimits[k]
			if limit == nil {
				limit = new(rateLimit)
				c.rateLimits[k] = limit
			}
			if n, err := strconv.Atoi(resp.Header.Get(rateLimitRemainingHeader)); err == nil {
				limit.remaining = n
			}
			if n, err := strconv.ParseFloat(resp.Header.Get(rateLimitResetAfterHeader), 64); err == nil {
				// Use Reset-After header to use local monotonic clock, since we may not
				// be in sync with Discord servers.
				limit.reset = time.Now().Add(time.Duration(n * float64(time.Second)))
			}
			c.mu.Unlock()
		} else if len(resp.Header[rateLimitGlobalHeader]) > 0 {
			c.mu.Lock()
			retryAfter := resp.Header.Get(retryAfterHeaderName)
			if n, err := strconv.Atoi(retryAfter); err == nil {
				c.globalRateLimitReset = time.Now().Add(time.Duration(n) * time.Second)
			} else if t, err := time.Parse(http.TimeFormat, retryAfter); err == nil {
				c.globalRateLimitReset = t
			}
			c.mu.Unlock()
		}

		// Reboot and try again if rate-limited.
		if resp.StatusCode != http.StatusTooManyRequests {
			break
		}
		span.AddEvent(fmt.Sprintf("Hit rate limit on %s (bucket=%q)", req.rateLimitRoute(), bucket))
	}

	span.SetAttributes(semconv.HTTPAttributesFromHTTPStatusCode(resp.StatusCode)...)
	if !(200 <= resp.StatusCode && resp.StatusCode < 300) {
		defer resp.Body.Close()
		typeHeader := resp.Header.Get(contentTypeHeaderName)
		if ct, _, err := mime.ParseMediaType(typeHeader); err != nil || ct != jsonMediaType {
			return nil, fmt.Errorf("%s: http %s", errURL, resp.Status)
		}
		const maxErrorSize = 1 << 20 // 1 MiB
		body, err := readBody(resp, maxErrorSize)
		if err != nil {
			return nil, fmt.Errorf("%s: http %s", errURL, resp.Status)
		}
		var parsedError struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if err := unmarshalJSON(body, &parsedError); err != nil {
			return nil, fmt.Errorf("%s: http %s", errURL, resp.Status)
		}
		return nil, fmt.Errorf("%s: Discord error code %d: %s", errURL, parsedError.Code, parsedError.Message)
	}
	return resp, nil
}

// An ApplicationCommand is the base "command" model that belongs to an application.
// https://discord.com/developers/docs/interactions/slash-commands#application-command-object-application-command-option-structure
type ApplicationCommand struct {
	ID            string                      `json:"id,omitempty"`
	ApplicationID string                      `json:"application_id"`
	GuildID       string                      `json:"guild_id,omitempty"`
	Name          string                      `json:"name"`
	Description   string                      `json:"description"`
	Options       []*ApplicationCommandOption `json:"options"`
}

// ApplicationCommandOptionType is an enumeration of the types of application
// command options.
// https://discord.com/developers/docs/interactions/slash-commands#application-command-object-application-command-option-type
type ApplicationCommandOptionType int

// Valid application command option types.
// https://discord.com/developers/docs/interactions/slash-commands#application-command-object-application-command-option-type
const (
	OptionTypeSubCommand      ApplicationCommandOptionType = 1
	OptionTypeSubCommandGroup ApplicationCommandOptionType = 2
	OptionTypeString          ApplicationCommandOptionType = 3
	OptionTypeInteger         ApplicationCommandOptionType = 4
	OptionTypeBoolean         ApplicationCommandOptionType = 5
	OptionTypeUser            ApplicationCommandOptionType = 6
	OptionTypeChannel         ApplicationCommandOptionType = 7
	OptionTypeRole            ApplicationCommandOptionType = 8
	OptionTypeMentionable     ApplicationCommandOptionType = 9
	OptionTypeNumber          ApplicationCommandOptionType = 10
)

// An ApplicationCommandOption describes an option given to an application command.
// https://discord.com/developers/docs/interactions/slash-commands#application-command-object-application-command-option-structure
type ApplicationCommandOption struct {
	Type        ApplicationCommandOptionType      `json:"type"`
	Name        string                            `json:"name"`
	Description string                            `json:"string"`
	Required    bool                              `json:"required"`
	Choices     []*ApplicationCommandOptionChoice `json:"choices"`
	Options     []*ApplicationCommandOption       `json:"options,omitempty"`
}

// An ApplicationCommandOptionChoice describes a valid value for a "choices"
// application command option.
// https://discord.com/developers/docs/interactions/slash-commands#application-command-object-application-command-option-choice-structure
type ApplicationCommandOptionChoice struct {
	Name  string      `json:"name"`
	Value interface{} `json:"value"` // a string or a json.Number
}

// ListGlobalApplicationCommands fetches all of the global commands for your
// application.
// https://discord.com/developers/docs/interactions/slash-commands#get-global-application-commands
func (c *Client) ListGlobalApplicationCommands(ctx context.Context, appID string) ([]*ApplicationCommand, error) {
	resp, err := c.do(ctx, &apiRequest{
		method:   http.MethodGet,
		route:    "/applications/{application.id}/commands",
		pathVars: map[string]string{"application.id": appID},
	})
	if err != nil {
		return nil, fmt.Errorf("list global application commands for %q: %v", appID, err)
	}
	body, err := readBody(resp, maxResponseSize)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("list global application commands for %q: %v", appID, err)
	}
	var parsed []*ApplicationCommand
	if err := unmarshalJSON(body, &parsed); err != nil {
		return nil, fmt.Errorf("list global application commands for %q: %v", appID, err)
	}
	return parsed, nil
}

// CreateGlobalApplicationCommand creates a new global command.
// https://discord.com/developers/docs/interactions/slash-commands#create-global-application-command
func (c *Client) CreateGlobalApplicationCommand(ctx context.Context, cmd *ApplicationCommand) (id string, err error) {
	reqBody, err := json.Marshal(cmd)
	if err != nil {
		return "", fmt.Errorf("create global application command %q for %q: %v", cmd.Name, cmd.ApplicationID, err)
	}
	resp, err := c.do(ctx, &apiRequest{
		method:   http.MethodPost,
		route:    "/applications/{application.id}/commands",
		pathVars: map[string]string{"application.id": cmd.ApplicationID},
		jsonData: reqBody,
	})
	if err != nil {
		return "", fmt.Errorf("create global application command %q for %q: %v", cmd.Name, cmd.ApplicationID, err)
	}
	body, err := readBody(resp, maxResponseSize)
	resp.Body.Close()
	if err != nil {
		return "", fmt.Errorf("create global application command %q for %q: %v", cmd.Name, cmd.ApplicationID, err)
	}
	var parsed ApplicationCommand
	if err := unmarshalJSON(body, &parsed); err != nil {
		return "", fmt.Errorf("create global application command %q for %q: %v", cmd.Name, cmd.ApplicationID, err)
	}
	return parsed.ID, nil
}

// https://discord.com/developers/docs/resources/user#user-object
type User struct {
	ID            string `json:"id"`
	Username      string `json:"username"`
	Discriminator string `json:"discriminator"`
	Avatar        string `json:"avatar,omitempty"`
	Bot           bool   `json:"bot"`
	System        bool   `json:"system"`
	MFAEnabled    bool   `json:"mfa_enabled"`
	Locale        string `json:"locale,omitempty"`
	Verified      bool   `json:"verified"`
	Email         string `json:"email,omitempty"`
}

// String returns the Discord handle in the standard format (e.g. "example#1234").
func (u *User) String() string {
	if u == nil {
		return "<nil>"
	}
	return u.Username + "#" + u.Discriminator
}

// GetUser returns a user object for the given ID.
// https://discord.com/developers/docs/resources/user#get-user
func (c *Client) GetUser(ctx context.Context, id string) (*User, error) {
	resp, err := c.do(ctx, &apiRequest{
		method:   http.MethodGet,
		route:    "/users/{user.id}",
		pathVars: map[string]string{"user.id": id},
	})
	if err != nil {
		return nil, fmt.Errorf("get user %q: %w", id, err)
	}
	body, err := readBody(resp, maxResponseSize)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("get user %q: %v", id, err)
	}
	parsed := new(User)
	if err := unmarshalJSON(body, parsed); err != nil {
		return nil, fmt.Errorf("get user %q: %v", id, err)
	}
	return parsed, nil
}

// https://discord.com/developers/docs/resources/guild#guild-member-object
type GuildMember struct {
	User        *User     `json:"user"`
	Nickname    string    `json:"nick,omitempty"`
	Roles       []string  `json:"roles"`
	JoinedAt    time.Time `json:"joined_at"`
	Deaf        bool      `json:"deaf"`
	Mute        bool      `json:"mute"`
	Pending     bool      `json:"pending"`
	Permissions string    `json:"permissions,omitempty"`
}

// https://discord.com/developers/docs/interactions/slash-commands#interaction-response-object-interaction-response-structure
type InteractionResponse struct {
	InteractionID    string `json:"-"`
	InteractionToken string `json:"-"`

	Type InteractionCallbackType  `json:"type"`
	Data *InteractionResponseData `json:"data,omitempty"`
}

// https://discord.com/developers/docs/interactions/slash-commands#interaction-response-object-interaction-application-command-callback-data-structure
type InteractionResponseData struct {
	TextToSpeech bool                     `json:"tts"`
	Content      string                   `json:"content,omitempty"`
	Flags        InteractionCallbackFlags `json:"flags"`
}

// InteractionCallbackType is an enumeration of interaction response types.
// https://discord.com/developers/docs/interactions/slash-commands#interaction-response-object-interaction-callback-type
type InteractionCallbackType int

// Valid interaction callback types.
// https://discord.com/developers/docs/interactions/slash-commands#interaction-response-object-interaction-callback-type
const (
	InteractionCallbackTypePong                             InteractionCallbackType = 1
	InteractionCallbackTypeChannelMessageWithSource         InteractionCallbackType = 4
	InteractionCallbackTypeDeferredChannelMessageWithSource InteractionCallbackType = 5
	InteractionCallbackTypeDeferredUpdateMessage            InteractionCallbackType = 6
	InteractionCallbackTypeUpdateMessage                    InteractionCallbackType = 7
)

// https://discord.com/developers/docs/interactions/slash-commands#interaction-response-object-interaction-application-command-callback-data-flags
type InteractionCallbackFlags uint

// Valid interaction callback
// https://discord.com/developers/docs/interactions/slash-commands#interaction-response-object-interaction-application-command-callback-data-flags
const (
	InteractionCallbackFlagEphemeral InteractionCallbackFlags = 1 << 6
)

func (c *Client) CreateInteractionResponse(ctx context.Context, resp *InteractionResponse) error {
	reqBody, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("create interaction response: %v", err)
	}
	httpResp, err := c.do(ctx, &apiRequest{
		method: http.MethodPost,
		route:  "/interactions/{interaction.id}/{interaction.token}/callback",
		pathVars: map[string]string{
			"interaction.id":    resp.InteractionID,
			"interaction.token": resp.InteractionToken,
		},
		jsonData: reqBody,
	})
	if err != nil {
		return fmt.Errorf("create interaction response: %v", err)
	}
	httpResp.Body.Close()
	return nil
}

// https://discord.com/developers/docs/resources/webhook#edit-webhook-message-jsonform-params
type EditWebhookMessageParams struct {
	WebhookID    string `json:"-"`
	WebhookToken string `json:"-"`
	MessageID    string `json:"-"`

	Content    string       `json:"content,omitempty"`
	Components []*Component `json:"components"`
}

// InteractionResponseID is the message ID to use when accessing or modifying
// the initial response to an interaction.
// https://discord.com/developers/docs/interactions/receiving-and-responding#followup-messages
const InteractionResponseID = "@original"

// EditWebhookMessage updates a message previously sent by a webhook or in
// response to an interaction.
// https://discord.com/developers/docs/resources/webhook#edit-webhook-message
func (c *Client) EditWebhookMessage(ctx context.Context, resp *EditWebhookMessageParams) (*Message, error) {
	reqBody, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("edit webhook message %q: %v", resp.MessageID, err)
	}
	httpResp, err := c.do(ctx, &apiRequest{
		method: http.MethodPatch,
		route:  "/webhooks/{webhook.id}/{webhook.token}/messages/{message.id}",
		pathVars: map[string]string{
			"webhook.id":    resp.WebhookID,
			"webhook.token": resp.WebhookToken,
			"message.id":    resp.MessageID,
		},
		jsonData: reqBody,
	})
	if err != nil {
		return nil, fmt.Errorf("edit webhook message %q: %v", resp.MessageID, err)
	}
	body, err := readBody(httpResp, maxResponseSize)
	httpResp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("edit webhook message %q: %v", resp.MessageID, err)
	}
	parsed := new(Message)
	if err := unmarshalJSON(body, parsed); err != nil {
		return nil, fmt.Errorf("edit webhook message %q: %v", resp.MessageID, err)
	}
	return parsed, nil
}

// A Channel represents a guild or DM channel within Discord.
// https://discord.com/developers/docs/resources/channel#channel-object
type Channel struct {
	ID                      string      `json:"id"`
	Type                    ChannelType `json:"type"`
	GuildID                 string      `json:"guild_id,omitempty"`
	Position                int         `json:"position,omitempty"`
	Name                    string      `json:"name,omitempty"`
	Topic                   string      `json:"topic,omitempty"`
	NSFW                    bool        `json:"nsfw,omitempty"`
	LastMessageID           string      `json:"last_message_id,omitempty"`
	Bitrate                 int         `json:"bitrate,omitempty"`
	UserLimit               int         `json:"user_limit,omitempty"`
	RateLimitPerUserSeconds int         `json:"rate_limit_per_user,omitempty"`
	Recipients              []*User     `json:"recipients,omitempty"`
	Icon                    string      `json:"icon,omitempty"`
	OwnerID                 string      `json:"owner_id,omitempty"`
	ApplicationID           string      `json:"application_id,omitempty"`
	ParentID                string      `json:"parent_id,omitempty"`
}

// ChannelType is an enumeration of channel types.
// https://discord.com/developers/docs/resources/channel#channel-object-channel-types
type ChannelType int

// Valid channel types.
// https://discord.com/developers/docs/resources/channel#channel-object-channel-types
const (
	ChannelTypeGuildText          ChannelType = 0
	ChannelTypeDM                 ChannelType = 1
	ChannelTypeGuildVoice         ChannelType = 2
	ChannelTypeGroupDM            ChannelType = 3
	ChannelTypeGuildCategory      ChannelType = 4
	ChannelTypeGuildNews          ChannelType = 5
	ChannelTypeGuildStore         ChannelType = 6
	ChannelTypeGuildNewsThread    ChannelType = 10
	ChannelTypeGuildPublicThread  ChannelType = 11
	ChannelTypeGuildPrivateThread ChannelType = 12
	ChannelTypeGuildStageVoice    ChannelType = 12
)

// GetChannel gets a channel by ID.
// https://discord.com/developers/docs/resources/user#create-dm
func (c *Client) GetChannel(ctx context.Context, id string) (*Channel, error) {
	resp, err := c.do(ctx, &apiRequest{
		method:   http.MethodGet,
		route:    "/channels/{channel.id}",
		pathVars: map[string]string{"channel.id": id},
	})
	if err != nil {
		return nil, fmt.Errorf("get channel info for %q: %v", id, err)
	}
	body, err := readBody(resp, maxResponseSize)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("get channel info for %q: %v", id, err)
	}
	parsed := new(Channel)
	if err := unmarshalJSON(body, parsed); err != nil {
		return nil, fmt.Errorf("get channel info for %q: %v", id, err)
	}
	return parsed, nil
}

// CreateDM creates a new DM channel with a user.
// https://discord.com/developers/docs/resources/user#create-dm
func (c *Client) CreateDM(ctx context.Context, recipientID string) (*Channel, error) {
	reqBody, err := json.Marshal(map[string]string{
		"recipient_id": recipientID,
	})
	if err != nil {
		return nil, fmt.Errorf("create DM channel to %q: %v", recipientID, err)
	}
	resp, err := c.do(ctx, &apiRequest{
		method:   http.MethodPost,
		route:    "/users/@me/channels",
		jsonData: reqBody,
	})
	if err != nil {
		return nil, fmt.Errorf("create DM channel to %q: %v", recipientID, err)
	}
	body, err := readBody(resp, maxResponseSize)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("create DM channel to %q: %v", recipientID, err)
	}
	parsed := new(Channel)
	if err := unmarshalJSON(body, parsed); err != nil {
		return nil, fmt.Errorf("create DM channel to %q: %v", recipientID, err)
	}
	return parsed, nil
}

// A Message represents a message sent in a channel within Discord.
// https://discord.com/developers/docs/resources/channel#message-object
type Message struct {
	ID                string            `json:"id"`
	ChannelID         string            `json:"channel_id"`
	GuildID           string            `json:"guild_id"`
	Author            *User             `json:"author,omitempty"`
	Member            *GuildMember      `json:"member,omitempty"`
	Content           string            `json:"content"`
	Timestamp         time.Time         `json:"timestamp"`
	TextToSpeech      bool              `json:"tts"`
	MentionEveryone   bool              `json:"mention_everyone"`
	Reactions         []*Reaction       `json:"reactions"`
	Nonce             string            `json:"nonce,omitempty"`
	Pinned            bool              `json:"pinned"`
	WebhookID         string            `json:"webhook_id"`
	Type              MessageType       `json:"type"`
	ApplicationID     string            `json:"application_id,omitempty"`
	ReferencedMessage *MessageReference `json:"referenced_message"`
	Thread            *Channel          `json:"thread,omitempty"`
	Components        []*Component      `json:"components,omitempty"`
}

func (msg *Message) User() *User {
	if msg == nil {
		return nil
	}
	if msg.Author != nil {
		return msg.Author
	}
	if msg.Member != nil {
		return msg.Member.User
	}
	return nil
}

// MessageType is an enumeration of message types.
// https://discord.com/developers/docs/resources/channel#message-object-message-types
type MessageType int

// Valid message types.
// https://discord.com/developers/docs/resources/channel#message-object-message-types
const (
	MessageTypeDefault MessageType = 0
)

// Reaction is an emoji reaction to a
// https://discord.com/developers/docs/resources/channel#reaction-object
type Reaction struct {
	Count int    `json:"count"`
	Me    bool   `json:"me"`
	Emoji *Emoji `json:"emoji"`
}

// A MessageReference is an attribution of a previous message.
// https://discord.com/developers/docs/resources/channel#message-reference-object-message-reference-structure
type MessageReference struct {
	MessageID         string `json:"message_id,omitempty"`
	ResponseMessageID string `json:"id,omitempty"` // undocumented, but on response we only get "id", not "message_id"
	ChannelID         string `json:"channel_id,omitempty"`
	GuildID           string `json:"guild_id,omitempty"`
}

// ID returns the first non-empty of ref.MessageID or ref.ResponseMessageID.
func (ref *MessageReference) ID() string {
	if ref == nil {
		return ""
	}
	if ref.MessageID != "" {
		return ref.MessageID
	}
	return ref.ResponseMessageID
}

// https://discord.com/developers/docs/resources/emoji#emoji-object-emoji-structure
type Emoji struct {
	ID             string `json:"id,omitempty"`
	Name           string `json:"name"`
	User           *User  `json:"user,omitempty"`
	RequiresColons bool   `json:"requires_colons,omitempty"`
	Managed        bool   `json:"managed,omitempty"`
	Animated       bool   `json:"animated"`
	Available      bool   `json:"available,omitempty"`
}

// A Component is an interactive element added to a Message.
// https://discord.com/developers/docs/interactions/message-components#component-object-component-structure
type Component struct {
	Type     ComponentType `json:"type"`
	CustomID string        `json:"custom_id,omitempty"`
	Disabled bool          `json:"disabled,omitempty"`

	ButtonStyle ButtonStyle `json:"style,omitempty"`
	ButtonLabel string      `json:"label,omitempty"`
	ButtonEmoji *Emoji      `json:"emoji,omitempty"`
	ButtonURL   string      `json:"url,omitempty"`

	SelectOptions     []*SelectOption `json:"options,omitempty"`
	SelectPlaceholder string          `json:"placeholder,omitempty"`
	SelectMinValues   json.Number     `json:"min_values,omitempty"`
	SelectMaxValues   json.Number     `json:"max_values,omitempty"`

	Components []*Component `json:"components,omitempty"`
}

// ComponentType is an enumeration of component types.
// https://discord.com/developers/docs/interactions/message-components#component-object-component-types
type ComponentType int

// Valid component types.
// https://discord.com/developers/docs/interactions/message-components#component-object-component-types
const (
	ComponentTypeActionRow  ComponentType = 1
	ComponentTypeButton     ComponentType = 2
	ComponentTypeSelectMenu ComponentType = 3
)

// ButtonStyle is an enumeration of component button styles.
// https://discord.com/developers/docs/interactions/message-components#button-object-button-styles
type ButtonStyle int

// Valid component button styles.
// https://discord.com/developers/docs/interactions/message-components#button-object-button-styles
const (
	ButtonStylePrimary   ButtonStyle = 1
	ButtonStyleSecondary ButtonStyle = 2
	ButtonStyleSuccess   ButtonStyle = 3
	ButtonStyleDanger    ButtonStyle = 4
	ButtonStyleLink      ButtonStyle = 5
)

// SelectOption is a single choice in a select menu component.
// https://discord.com/developers/docs/interactions/message-components#select-menu-object-select-option-structure
type SelectOption struct {
	Label       string `json:"label"`
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
	Emoji       *Emoji `json:"emoji,omitempty"`
	Default     bool   `json:"default,omitempty"`
}

// https://discord.com/developers/docs/resources/channel#create-message-jsonform-params
type CreateMessageParams struct {
	ChannelID        string            `json:"-"`
	Content          string            `json:"content"`
	TextToSpeech     bool              `json:"tts"`
	MessageReference *MessageReference `json:"message_reference,omitempty"`
	Components       []*Component      `json:"components,omitempty"`
}

// GetMessage returns a specific message in the channel.
// https://discord.com/developers/docs/resources/channel#get-channel-message
func (c *Client) GetMessage(ctx context.Context, channelID, messageID string) (*Message, error) {
	resp, err := c.do(ctx, &apiRequest{
		method: http.MethodGet,
		route:  "/channels/{channel.id}/messages/{message.id}",
		pathVars: map[string]string{
			"channel.id": channelID,
			"message.id": messageID,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("get message %q: %v", messageID, err)
	}
	body, err := readBody(resp, maxResponseSize)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("get message %q: %v", messageID, err)
	}
	parsed := new(Message)
	if err := unmarshalJSON(body, parsed); err != nil {
		return nil, fmt.Errorf("get message %q: %v", messageID, err)
	}
	return parsed, nil
}

// CreateMessage posts a message to a guild text or DM channel.
// https://discord.com/developers/docs/resources/channel#create-message
func (c *Client) CreateMessage(ctx context.Context, params *CreateMessageParams) (*Message, error) {
	reqBody, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("send message to channel %q: %v", params.ChannelID, err)
	}
	resp, err := c.do(ctx, &apiRequest{
		method:   http.MethodPost,
		route:    "/channels/{channel.id}/messages",
		pathVars: map[string]string{"channel.id": params.ChannelID},
		jsonData: reqBody,
	})
	if err != nil {
		return nil, fmt.Errorf("send message to channel %q: %v", params.ChannelID, err)
	}
	body, err := readBody(resp, maxResponseSize)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("send message to channel %q: %v", params.ChannelID, err)
	}
	parsed := new(Message)
	if err := unmarshalJSON(body, parsed); err != nil {
		return nil, fmt.Errorf("send message to channel %q: %v", params.ChannelID, err)
	}
	return parsed, nil
}

// https://discord.com/developers/docs/resources/channel#edit-message-jsonform-params
type EditMessageParams struct {
	ChannelID  string       `json:"-"`
	MessageID  string       `json:"-"`
	Content    string       `json:"content,omitempty"`
	Components []*Component `json:"components"`
}

// EditMessage posts a message to a guild text or DM channel.
// https://discord.com/developers/docs/resources/channel#edit-message
func (c *Client) EditMessage(ctx context.Context, params *EditMessageParams) (*Message, error) {
	reqBody, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("edit message %q in channel %q: %v", params.MessageID, params.ChannelID, err)
	}
	resp, err := c.do(ctx, &apiRequest{
		method: http.MethodPatch,
		route:  "/channels/{channel.id}/messages/{message.id}",
		pathVars: map[string]string{
			"channel.id": params.ChannelID,
			"message.id": params.MessageID,
		},
		jsonData: reqBody,
	})
	if err != nil {
		return nil, fmt.Errorf("edit message %q in channel %q: %v", params.MessageID, params.ChannelID, err)
	}
	body, err := readBody(resp, maxResponseSize)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("edit message %q in channel %q: %v", params.MessageID, params.ChannelID, err)
	}
	parsed := new(Message)
	if err := unmarshalJSON(body, parsed); err != nil {
		return nil, fmt.Errorf("edit message %q in channel %q: %v", params.MessageID, params.ChannelID, err)
	}
	return parsed, nil
}

// CreateReaction creates a reaction for a message to a guild text or DM channel.
// https://discord.com/developers/docs/resources/channel#create-reaction
func (c *Client) CreateReaction(ctx context.Context, channelID, messageID, emoji string) error {
	httpResp, err := c.do(ctx, &apiRequest{
		method: http.MethodPut,
		route:  "/channels/{channel.id}/messages/{message.id}/reactions/{emoji}/@me",
		pathVars: map[string]string{
			"channel.id": channelID,
			"message.id": messageID,
			"emoji":      emoji,
		},
	})
	if err != nil {
		return fmt.Errorf("create reaction %q on message %q: %w", emoji, messageID, err)
	}
	httpResp.Body.Close()
	return nil
}

type Permissions uint64

const (
	SendMessages Permissions = 1 << 11
)

func readBody(resp *http.Response, maxSize int) ([]byte, error) {
	if resp.ContentLength > int64(maxSize) {
		return nil, fmt.Errorf("response body too large (%d bytes)", resp.ContentLength)
	}
	var b []byte
	if maxSize < 512 {
		b = make([]byte, 0, maxSize+1)
	} else {
		b = make([]byte, 0, 512)
	}
	for {
		if len(b) == cap(b) {
			// Add more capacity.
			newCap := int64(len(b)) * 2
			if newCap > int64(maxSize)+1 {
				newCap = int64(maxSize) + 1
			}
			b2 := make([]byte, len(b), newCap)
			copy(b2, b)
			b = b2
		}
		n, err := resp.Body.Read(b[len(b):cap(b)])
		b = b[:len(b)+n]
		if len(b) > maxSize {
			return b[:maxSize], fmt.Errorf("response body too large (stopped at %d bytes)", len(b))
		}
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return b, err
		}
	}
}

func unmarshalJSON(data []byte, value interface{}) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	return d.Decode(value)
}

func defaultBaseURL() *url.URL {
	return &url.URL{
		Scheme: "https",
		Host:   "discord.com",
		Path:   "/api/v9",
	}
}

var cachedModuleVersion struct {
	val string
	sync.Once
}

func moduleVersion() string {
	cachedModuleVersion.Do(func() {
		cachedModuleVersion.val = "development" // default
		info, ok := debug.ReadBuildInfo()
		if !ok {
			return
		}
		const wantPath = "zombiezen.com/go/discord"
		if info.Main.Path == wantPath {
			if info.Main.Version != "" {
				cachedModuleVersion.val = info.Main.Version
			}
			return
		}
		for _, mod := range info.Deps {
			if mod.Path == wantPath && mod.Version != "" {
				cachedModuleVersion.val = mod.Version
				return
			}
		}
	})
	return cachedModuleVersion.val
}
