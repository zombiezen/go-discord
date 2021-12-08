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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"mime"
	"net/http"
	"net/url"
	"runtime"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/sync/errgroup"
)

// Gateway represents an ongoing stream of events from the Discord Gateway.
// https://discord.com/developers/docs/topics/gateway
type Gateway struct {
	url       url.URL
	token     string
	userAgent string

	conn              *websocket.Conn
	heartbeat         *time.Ticker
	heartbeatInterval time.Duration
	pendingAck        bool            // whether the server has acknowledged the last heartbeat
	bufferedEvent     *GatewayPayload // buffered event from ensureConn

	// Session resumption
	sessionID      string
	sequenceNumber int64

	// Diagnostics callbacks
	errFunc            func(context.Context, error)
	connectAttemptFunc func(context.Context, bool)
	connectSuccessFunc func(context.Context, bool)
}

// OpenGateway discovers the gateway URL and returns a new Gateway.
// The caller is responsible for calling Close on the returned Gateway.
func (c *Client) OpenGateway(ctx context.Context) (*Gateway, error) {
	resp, err := c.do(ctx, &apiRequest{
		method: http.MethodGet,
		route:  "/gateway",
	})
	if err != nil {
		return nil, fmt.Errorf("find gateway URL: %v", err)
	}
	defer resp.Body.Close()
	contentTypeHeader := resp.Header.Get(contentTypeHeaderName)
	if ct, _, err := mime.ParseMediaType(contentTypeHeader); err != nil || ct != jsonMediaType {
		return nil, fmt.Errorf("find gateway URL: response is %q instead of JSON", contentTypeHeader)
	}
	data, err := readBody(resp, maxResponseSize)
	if err != nil {
		return nil, fmt.Errorf("find gateway URL: %v", err)
	}
	var parsed struct {
		RawURL string `json:"url"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("find gateway URL: %v", err)
	}
	u, err := url.Parse(parsed.RawURL)
	if err != nil {
		return nil, fmt.Errorf("find gateway URL: %v", err)
	}
	q := u.Query()
	q.Set("v", "9")
	q.Set("encoding", "json")
	u.RawQuery = q.Encode()
	g := &Gateway{
		url:       *u,
		token:     c.auth.token(),
		userAgent: c.userAgent,
	}
	g.SetErrorFunc(nil)
	g.SetConnectFuncs(nil, nil)
	return g, nil
}

// URL returns the gateway's URL.
func (g *Gateway) URL() *url.URL {
	return &g.url
}

// SetErrorFunc sets a callback that is called
// when a non-critical error occurs during listening.
func (g *Gateway) SetErrorFunc(f func(ctx context.Context, err error)) {
	if f == nil {
		g.errFunc = func(context.Context, error) {}
	} else {
		g.errFunc = f
	}
}

// SetConnectFuncs sets callbacks that are called
// when a Websocket connection is about to be opened
// or after it is has successfully established.
func (g *Gateway) SetConnectFuncs(start, success func(ctx context.Context, isResume bool)) {
	if start == nil {
		g.connectAttemptFunc = func(context.Context, bool) {}
	} else {
		g.connectAttemptFunc = start
	}
	if success == nil {
		g.connectSuccessFunc = func(context.Context, bool) {}
	} else {
		g.connectSuccessFunc = success
	}
}

// WaitForReady waits for the gateway connection to be ready.
func (g *Gateway) WaitForReady(ctx context.Context) error {
	if err := g.ensureConn(ctx); err != nil {
		return fmt.Errorf("waiting for Discord gateway: %v", err)
	}
	return nil
}

// Listen listens for the next event from the gateway.
//
// If there is a long delay (>30 seconds) between calls to Listen,
// the gateway may drop the connection.
// Listen will automatically handle any reconnects,
// so Listen will only return an error if the Context is Done.
func (g *Gateway) Listen(ctx context.Context) (*GatewayPayload, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("listen for Discord event: %v", ctx.Err())
		default:
		}
		if err := g.ensureConn(ctx); err != nil {
			return nil, fmt.Errorf("listen for Discord event: %v", err)
		}
		if g.bufferedEvent != nil {
			msg := g.bufferedEvent
			g.bufferedEvent = nil
			g.updateSession(ctx, msg)
			if msg.EventName == resumedEvent {
				debugf("Skipping %s event", resumedEvent)
				continue
			}
			return msg, nil
		}
		msg, err := g.next(ctx)
		if err != nil {
			g.errFunc(ctx, fmt.Errorf("discord gateway: %w", err))
			if err := g.closeWithCode(websocket.CloseProtocolError, "invalid sequence"); err != nil {
				g.errFunc(ctx, fmt.Errorf("disconnect discord gateway websocket: %w", err))
			}
			continue
		}
		switch msg.OpCode {
		case gatewayDispatchOpcode:
			g.updateSession(ctx, msg)
			if msg.EventName == resumedEvent {
				debugf("Skipping %s event", resumedEvent)
				continue
			}
			return msg, nil
		case gatewayReconnectOpcode, gatewayInvalidSessionOpcode:
			// From https://discord.com/developers/docs/topics/gateway#invalid-session:
			// "The inner d key is a boolean that indicates whether the session may be resumable."
			if msg.OpCode == gatewayInvalidSessionOpcode && !bytes.Equal(msg.Data, []byte("true")) {
				g.sessionID = ""
			}
			debugf("Gateway requested reconnect (op=%d data=%s)", msg.OpCode, msg.Data)
			if err := g.closeWithCode(websocket.CloseAbnormalClosure, "reconnecting"); err != nil {
				debugf("Disconnect websocket: %v", err)
			}
		default:
			g.errFunc(ctx, fmt.Errorf("discord gateway: received unhandled opcode %d", msg.OpCode))
			if err := g.closeWithCode(websocket.CloseProtocolError, "invalid sequence"); err != nil {
				debugf("Disconnect websocket: %v", err)
			}
		}
	}
}

// updateSession updates the stored session information
// based on the most recently received payload.
func (g *Gateway) updateSession(ctx context.Context, msg *GatewayPayload) {
	if msg.OpCode != gatewayDispatchOpcode {
		return
	}
	if msg.EventName == readyEvent {
		var readyData struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(msg.Data, &readyData); err != nil {
			g.errFunc(ctx, fmt.Errorf("discord gateway: could not determine session from %s event: %v", readyEvent, err))
			g.sessionID = ""
			return
		}
		g.sessionID = readyData.SessionID
	}
	g.sequenceNumber = msg.SequenceNumber
}

// next waits for the next non-heartbeat payload on the current connection.
func (g *Gateway) next(ctx context.Context) (*GatewayPayload, error) {
	acks := make(chan struct{})
	serverHeartbeats := make(chan struct{})
	eventReceived := errors.New("event received")

	grp, ctx := errgroup.WithContext(ctx)
	grp.Go(func() error {
		// https://discord.com/developers/docs/topics/gateway#heartbeating
		for {
			select {
			case <-g.heartbeat.C:
				if g.pendingAck {
					return fmt.Errorf("did not receive heartbeat from server")
				}
				if err := g.write(ctx, gatewayHeartbeatOpcode, nil); err != nil {
					return fmt.Errorf("send heartbeat: %w", err)
				}
				debugf("Sent heartbeat to gateway")
				g.pendingAck = true
			case <-serverHeartbeats:
				if err := g.write(ctx, gatewayHeartbeatOpcode, nil); err != nil {
					return fmt.Errorf("send heartbeat: %w", err)
				}
				debugf("Sent heartbeat to gateway")
				g.pendingAck = true
				g.heartbeat.Reset(g.heartbeatInterval)
			case <-acks:
				debugf("Received heartbeat ACK from gateway")
				g.pendingAck = false
			case <-ctx.Done():
				return nil
			}
		}
	})
	var msg *GatewayPayload
	grp.Go(func() error {
		for {
			var err error
			msg, err = g.read(ctx)
			if err != nil {
				return err
			}
			switch msg.OpCode {
			case gatewayHeartbeatOpcode:
				select {
				case serverHeartbeats <- struct{}{}:
				case <-ctx.Done():
					return ctx.Err()
				}
			case gatewayHeartbeatACKOpcode:
				select {
				case acks <- struct{}{}:
				case <-ctx.Done():
					return ctx.Err()
				}
			default:
				// Ending this goroutine should stop the heartbeat, so we use a
				// non-nil error.
				return eventReceived
			}
		}
	})
	if err := grp.Wait(); err != eventReceived {
		return nil, err
	}
	return msg, nil
}

// Close releases any resources associated with the gateway connection.
func (g *Gateway) Close() error {
	return g.closeWithCode(websocket.CloseGoingAway, "client disconnect")
}

func (g *Gateway) closeWithCode(code int, text string) error {
	if g.heartbeat != nil {
		g.heartbeat.Stop()
		g.heartbeat = nil
	}
	if g.conn == nil {
		return nil
	}
	msg := websocket.FormatCloseMessage(code, text)
	err1 := g.conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(10*time.Second))
	err2 := g.conn.Close()
	g.conn = nil
	if err1 != nil {
		return fmt.Errorf("close gateway: %w", err1)
	}
	if err2 != nil {
		return fmt.Errorf("close gateway: %w", err2)
	}
	return nil
}

func (g *Gateway) ensureConn(ctx context.Context) error {
	const retryInterval = 5 * time.Second
	var t *time.Timer
	for {
		err := g.connect(ctx)
		if err == nil {
			return nil
		}
		g.errFunc(ctx, fmt.Errorf("connect to Discord gateway (will retry in %v): %v", retryInterval, err))
		if t == nil {
			t = time.NewTimer(retryInterval)
			defer t.Stop()
		} else {
			t.Reset(retryInterval)
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			return err
		}
	}
}

func (g *Gateway) connect(ctx context.Context) (err error) {
	if g.conn != nil {
		return nil
	}

	g.connectAttemptFunc(ctx, g.sessionID != "")
	var resp *http.Response
	g.pendingAck = false
	g.bufferedEvent = nil
	g.conn, resp, err = new(websocket.Dialer).DialContext(ctx, g.url.String(), http.Header{
		userAgentHeaderName: {g.userAgent},
	})
	if err != nil {
		if resp != nil {
			return fmt.Errorf("connect to Discord gateway: http %s", resp.Status)
		}
		return fmt.Errorf("connect to Discord gateway: %v", err)
	}
	defer func() {
		if err != nil {
			closeErr := g.conn.Close()
			g.conn = nil
			if closeErr != nil {
				g.errFunc(ctx, fmt.Errorf("closing Discord gateway connection: %v", closeErr))
			}
		}
	}()

	// https://discord.com/developers/docs/topics/gateway#connecting-example-gateway-hello
	debugf("Started gateway websocket; reading hello...")
	hello, err := g.read(ctx)
	if err != nil {
		return fmt.Errorf("connect to Discord gateway: hello: %v", err)
	}
	if hello.OpCode != gatewayHelloOpcode {
		return fmt.Errorf(
			"connect to Discord gateway: hello: unexpected opcode %d (want %d)",
			hello.OpCode, gatewayHelloOpcode,
		)
	}
	var helloData struct {
		HeartbeatMillis int `json:"heartbeat_interval"`
	}
	if err := json.Unmarshal(hello.Data, &helloData); err != nil {
		return fmt.Errorf("connect to Discord gateway: hello: %v", err)
	}
	g.heartbeatInterval = time.Duration(helloData.HeartbeatMillis) * time.Millisecond
	debugf("Gateway heartbeat is %v", g.heartbeatInterval)
	g.heartbeat = time.NewTicker(g.heartbeatInterval)
	defer func() {
		if err != nil {
			g.heartbeat.Stop()
			g.heartbeat = nil
		}
	}()

	// https://discord.com/developers/docs/topics/gateway#resuming
	if g.sessionID != "" {
		debugf("Attempting to resume session=%q sequence=%d", g.sessionID, g.sequenceNumber)
		err = g.write(ctx, gatewayResumeOpcode, map[string]interface{}{
			"token":      g.token,
			"session_id": g.sessionID,
			"seq":        g.sequenceNumber,
		})
		if err != nil {
			return fmt.Errorf("connect to Discord gateway: resume: %v", err)
		}
		msg, err := g.next(ctx)
		if err != nil {
			return fmt.Errorf("connect to Discord gateway: resume response: %v", err)
		}
		switch msg.OpCode {
		case gatewayDispatchOpcode:
			// Success! Replaying missing events.
			g.connectSuccessFunc(ctx, true)
			g.bufferedEvent = msg
			return nil
		case gatewayInvalidSessionOpcode:
			// Identify as normal.
			sleep := time.Duration(1+rand.Intn(5)) * time.Second
			debugf("Gateway rejected session resumption. Re-identifying in %v", sleep)
			g.sessionID = ""
			t := time.NewTimer(sleep)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return fmt.Errorf("connect to Discord gateway: %v", ctx.Err())
			}
		default:
			return fmt.Errorf(
				"connect to Discord gateway: resume response: unhandled opcode %d (want %d or %d)",
				msg.OpCode, gatewayDispatchOpcode, gatewayInvalidSessionOpcode,
			)
		}
	}
	// https://discord.com/developers/docs/topics/gateway#identifying
	err = g.write(ctx, gatewayIdentifyOpcode, map[string]interface{}{
		"token": g.token,
		"properties": map[string]string{
			"$os":      runtime.GOOS,
			"$browser": g.userAgent,
			"$device":  g.userAgent,
		},
		// https://discord.com/developers/docs/topics/gateway#gateway-intents
		"intents": 1 << 12, // DIRECT_MESSAGES
	})
	if err != nil {
		return fmt.Errorf("connect to Discord gateway: identify: %v", err)
	}
	msg, err := g.next(ctx)
	if err != nil {
		return fmt.Errorf("connect to Discord gateway: identify response: %v", err)
	}
	if msg.OpCode != gatewayDispatchOpcode {
		return fmt.Errorf(
			"connect to Discord gateway: identify response: unexpected opcode %d (want %d)",
			msg.OpCode, gatewayDispatchOpcode,
		)
	}
	if msg.EventName != readyEvent {
		return fmt.Errorf("connect to Discord gateway: identify response: unexpected event %q", msg.EventName)
	}

	g.connectSuccessFunc(ctx, false)
	g.bufferedEvent = msg
	return nil
}

// Payload opcodes.
const (
	gatewayDispatchOpcode       = 0
	gatewayHeartbeatOpcode      = 1
	gatewayIdentifyOpcode       = 2
	gatewayResumeOpcode         = 6
	gatewayReconnectOpcode      = 7
	gatewayInvalidSessionOpcode = 9
	gatewayHelloOpcode          = 10
	gatewayHeartbeatACKOpcode   = 11
)

// Event names treated specially by the gateway client.
const (
	readyEvent   = "READY"
	resumedEvent = "RESUMED"
)

// GatewayPayload represents a single message sent over the Discord gateway websocket.
// https://discord.com/developers/docs/topics/gateway#payloads-gateway-payload-structure
type GatewayPayload struct {
	OpCode         int             `json:"op"`
	Data           json.RawMessage `json:"d"`
	SequenceNumber int64           `json:"s,omitempty"`
	EventName      string          `json:"t,omitempty"`
}

func (g *Gateway) read(ctx context.Context) (*GatewayPayload, error) {
	var msg []byte
	if ctxDone := ctx.Done(); ctxDone == nil {
		var err error
		_, msg, err = g.conn.ReadMessage()
		if err != nil {
			return nil, fmt.Errorf("read gateway payload: %v", err)
		}
	} else {
		select {
		case <-ctxDone:
			return nil, fmt.Errorf("read gateway payload: %v", ctx.Err())
		default:
		}
		read := make(chan struct{})
		watchDone := make(chan struct{})
		go func() {
			close(watchDone)
			select {
			case <-read:
			case <-ctxDone:
				g.conn.SetReadDeadline(time.Now())
			}
		}()
		var err error
		_, msg, err = g.conn.ReadMessage()
		close(read)
		<-watchDone
		if err != nil {
			return nil, fmt.Errorf("read gateway payload: %v", err)
		}
	}
	payload := new(GatewayPayload)
	if err := json.Unmarshal(msg, payload); err != nil {
		return nil, fmt.Errorf("read gateway payload: %v", err)
	}
	return payload, nil
}

func (g *Gateway) write(ctx context.Context, opCode int, data interface{}) error {
	payload := &GatewayPayload{
		OpCode: opCode,
	}
	var err error
	payload.Data, err = json.Marshal(data)
	if err != nil {
		return fmt.Errorf("send gateway payload: %v", err)
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("send gateway payload: %v", err)
	}

	ctxDone := ctx.Done()
	if ctxDone == nil {
		if err := g.conn.WriteMessage(websocket.TextMessage, payloadJSON); err != nil {
			return fmt.Errorf("send gateway payload: %v", err)
		}
		return nil
	}
	select {
	case <-ctxDone:
		return fmt.Errorf("send gateway payload: %v", ctx.Err())
	default:
	}
	written := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		close(watchDone)
		select {
		case <-written:
		case <-ctxDone:
			// XXX This is racy because WriteMessage will unconditionally call SetWriteDeadline.
			g.conn.UnderlyingConn().SetWriteDeadline(time.Now())
		}
	}()
	err = g.conn.WriteMessage(websocket.TextMessage, payloadJSON)
	close(written)
	<-watchDone
	if err != nil {
		return fmt.Errorf("send gateway payload: %v", err)
	}
	return nil
}

// An Interaction is the base "thing" that is sent when a user invokes a command,
// and is the same for Slash Commands and other future interaction types
// (such as Message Components).
// https://discord.com/developers/docs/interactions/slash-commands#interaction-object-interaction-structure
type Interaction struct {
	ID            string                 `json:"id"`
	ApplicationID string                 `json:"application_id"`
	Type          InteractionRequestType `json:"type"`
	Data          *InteractionData       `json:"data,omitempty"`
	GuildID       string                 `json:"guild_id,omitempty"`
	ChannelID     string                 `json:"channel_id,omitempty"`
	Member        *GuildMember           `json:"member,omitempty"`
	DMUser        *User                  `json:"user,omitempty"`
	Token         string                 `json:"token"`
	Message       *Message               `json:"message,omitempty"`
}

func (interaction *Interaction) User() *User {
	if interaction == nil {
		return nil
	}
	if interaction.DMUser != nil {
		return interaction.DMUser
	}
	if interaction.Member != nil {
		return interaction.Member.User
	}
	return nil
}

// InteractionRequestType is an enumeration of interaction request types.
// https://discord.com/developers/docs/interactions/slash-commands#interaction-object-interaction-request-type
type InteractionRequestType int

// Valid interaction request types.
// https://discord.com/developers/docs/interactions/slash-commands#interaction-object-interaction-request-type
const (
	InteractionRequestTypePing               InteractionRequestType = 1
	InteractionRequestTypeApplicationCommand InteractionRequestType = 2
	InteractionRequestTypeMessageComponent   InteractionRequestType = 3
)

// InteractionData holds data about an interaction.
// https://discord.com/developers/docs/interactions/slash-commands#interaction-object-application-command-interaction-data-structure
type InteractionData struct {
	ID            string                      `json:"id"`
	CommandName   string                      `json:"name"`
	Options       []*ApplicationCommandOption `json:"options,omitempty"`
	CustomID      string                      `json:"custom_id,omitempty"`
	ComponentType ComponentType               `json:"component_type,omitempty"`
	Values        []string                    `json:"values"`
}

// InteractionDataOption holds data about an interaction option chosen.
// https://discord.com/developers/docs/interactions/slash-commands#interaction-object-application-command-interaction-data-option-structure
type InteractionDataOption struct {
	Name    string                       `json:"name"`
	Type    ApplicationCommandOptionType `json:"type"`
	Value   interface{}                  `json:"value,omitempty"`
	Options []*InteractionDataOption     `json:"options,omitempty"`
}

func debugf(format string, args ...interface{}) {}
