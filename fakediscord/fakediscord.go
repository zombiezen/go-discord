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

// Package fakediscord provides an in-memory implementation of the Discord API.
package fakediscord

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	mathrand "math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"zombiezen.com/go/discord"
	"zombiezen.com/go/discord/snowflake"
	"zombiezen.com/go/log"
)

var discordEpoch = time.Unix(1420070400, 0)

// HTTP headers
const (
	authorizationHeaderName = "Authorization"
	contentLengthHeaderName = "Content-Length"
	contentTypeHeaderName   = "Content-Type"
	retryAfterHeaderName    = "Retry-After"
	userAgentHeaderName     = "User-Agent"
)

// A Server is an isolated instance of the Discord API.
// It implements http.Handler and serves at the path "/api".
// It is safe to call Server's methods from multiple goroutines.
// The zero value is an instance of the Discord API
// without any users or channels.
type Server struct {
	mu        sync.Mutex
	logger    log.Logger
	rand      *mathrand.Rand
	router    *mux.Router
	auths     map[discord.AuthHeader]*discord.User
	users     map[string]*discord.User
	channels  map[string]*discord.Channel
	messages  map[string]*discord.Message
	reactions map[string]map[string]map[*discord.User]struct{}
	gateways  map[*gateway]struct{}

	snowflake snowflake.Generator
}

// SetLogger sets the log that the server writes events to.
// The default is to discard logs.
func (srv *Server) SetLogger(logger log.Logger) {
	if logger == nil {
		logger = log.Discard
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	srv.logger = logger
}

func (srv *Server) lockedInit() error {
	if srv.rand != nil {
		return nil
	}
	if srv.logger == nil {
		srv.logger = log.Discard
	}
	var entropy [8]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return fmt.Errorf("init random: %v", err)
	}
	seed := int64(binary.LittleEndian.Uint64(entropy[:]))
	srv.rand = mathrand.New(mathrand.NewSource(seed))
	srv.snowflake.Init(discordEpoch, uint16(srv.rand.Uint32()))

	srv.router = mux.NewRouter()
	srv.fillRoutes(srv.router.PathPrefix("/api").Subrouter())
	srv.fillRoutes(srv.router.PathPrefix("/api/{version:v[0-9]+}").Subrouter())
	srv.router.HandleFunc("/gateway", srv.handleGatewayWebsocket)

	srv.auths = make(map[discord.AuthHeader]*discord.User)
	srv.users = make(map[string]*discord.User)
	srv.channels = make(map[string]*discord.Channel)
	srv.messages = make(map[string]*discord.Message)
	srv.reactions = make(map[string]map[string]map[*discord.User]struct{})
	srv.gateways = make(map[*gateway]struct{})

	return nil
}

func (srv *Server) fillRoutes(r *mux.Router) {
	r.Handle("/oauth2/@me", handlers.MethodHandler{
		http.MethodGet: apiHandlerFunc(srv.currentAuthInformation),
	})
	r.Handle("/gateway", handlers.MethodHandler{
		http.MethodGet: apiHandlerFunc(srv.getGateway),
	})
	r.Handle("/users/@me/channels", handlers.MethodHandler{
		http.MethodPost: apiHandlerFunc(srv.createUserChannel),
	})
	r.Handle("/channels/{channel.id}", handlers.MethodHandler{
		http.MethodGet: apiHandlerFunc(srv.getChannel),
	})
	r.Handle("/channels/{channel.id}/messages", handlers.MethodHandler{
		http.MethodPost: apiHandlerFunc(srv.createMessage),
	})
	r.Handle("/channels/{channel.id}/messages/{message.id}", handlers.MethodHandler{
		http.MethodGet: apiHandlerFunc(srv.getChannelMessage),
	})
	r.Handle("/channels/{channel.id}/messages/{message.id}/reactions/{emoji}/@me", handlers.MethodHandler{
		http.MethodPut: apiHandlerFunc(srv.createReaction),
	})
}

// ServeHTTP serves an API request.
func (srv *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	srv.mu.Lock()
	if err := srv.lockedInit(); err != nil {
		srv.mu.Unlock()
		writeErrorResponse(w, err)
		return
	}
	router := srv.router
	srv.mu.Unlock()

	w.Header().Set("X-RateLimit-Bucket", "wholebucket")
	w.Header().Set("X-RateLimit-Limit", "1000000")
	w.Header().Set("X-RateLimit-Remaining", "1000000")
	w.Header().Set("X-RateLimit-Reset-After", "1")
	resetTime := time.Now().Add(1 * time.Second).Unix()
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetTime, 10))
	router.ServeHTTP(w, r)
}

func (srv *Server) currentAuthInformation(ctx context.Context, r *apiRequest) (*apiResponse, error) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	authUser := srv.lockedRequestUser(r)
	if authUser == nil {
		return nil, errUnauthorized
	}
	return newResponse(http.StatusOK, map[string]interface{}{
		"user": authUser,
	})
}

func (srv *Server) createUserChannel(ctx context.Context, r *apiRequest) (*apiResponse, error) {
	var requestData struct {
		RecipientID string `json:"recipient_id"`
	}
	if err := json.Unmarshal(r.body, &requestData); err != nil {
		return nil, err
	}
	if requestData.RecipientID == "" {
		return nil, &apiError{
			httpStatusCode: http.StatusBadRequest,
			err:            errors.New("must specify recipient_id"),
		}
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	authUser := srv.lockedRequestUser(r)
	if authUser == nil {
		return nil, errUnauthorized
	}
	recipientUser := srv.users[requestData.RecipientID]
	if recipientUser == nil {
		return nil, &apiError{
			httpStatusCode: http.StatusBadRequest,
			err:            errors.New("unknown recipient"),
			discordCode:    10013,
		}
	}
	for _, c := range srv.channels {
		if len(c.Recipients) == 2 && hasRecipient(c, authUser) && hasRecipient(c, recipientUser) {
			return newResponse(http.StatusCreated, c)
		}
	}
	newChannel := &discord.Channel{
		ID:         strconv.FormatInt(int64(srv.snowflake.Generate()), 10),
		Type:       discord.ChannelTypeDM,
		Recipients: []*discord.User{authUser, recipientUser},
	}
	srv.channels[newChannel.ID] = newChannel
	return newResponse(http.StatusCreated, newChannel)
}

func (srv *Server) getChannel(ctx context.Context, r *apiRequest) (*apiResponse, error) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	authUser := srv.lockedRequestUser(r)
	if authUser == nil {
		return nil, errUnauthorized
	}
	channelID := r.pathVars["channel.id"]
	channel := srv.channels[channelID]
	if channel == nil || !hasRecipient(channel, authUser) {
		return nil, &apiError{
			discordCode:    10003,
			err:            fmt.Errorf("unknown channel %q", channelID),
			httpStatusCode: http.StatusNotFound,
		}
	}
	return newResponse(http.StatusOK, channel)
}

func (srv *Server) getChannelMessage(ctx context.Context, r *apiRequest) (*apiResponse, error) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	authUser := srv.lockedRequestUser(r)
	if authUser == nil {
		return nil, errUnauthorized
	}
	channelID := r.pathVars["channel.id"]
	channel := srv.channels[channelID]
	if channel == nil || !hasRecipient(channel, authUser) {
		return nil, &apiError{
			discordCode:    10003,
			err:            fmt.Errorf("unknown channel %q", channelID),
			httpStatusCode: http.StatusNotFound,
		}
	}
	messageID := r.pathVars["message.id"]
	message := srv.messages[messageID]
	if message == nil || message.ChannelID != channelID {
		return nil, &apiError{
			discordCode:    10008,
			err:            fmt.Errorf("unknown message %q", channelID),
			httpStatusCode: http.StatusNotFound,
		}
	}
	return newResponse(http.StatusOK, srv.lockedFormatMessageForUser(authUser, message))
}

func (srv *Server) createMessage(ctx context.Context, r *apiRequest) (*apiResponse, error) {
	requestData := &discord.CreateMessageParams{
		ChannelID: r.pathVars["channel.id"],
	}
	if err := json.Unmarshal(r.body, &requestData); err != nil {
		return nil, &apiError{
			err:            err,
			httpStatusCode: http.StatusBadRequest,
		}
	}
	if requestData.Content == "" {
		return nil, &apiError{
			discordCode:    50006,
			err:            errors.New("message is empty"),
			httpStatusCode: http.StatusBadRequest,
		}
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	authUser := srv.lockedRequestUser(r)
	if authUser == nil {
		return nil, errUnauthorized
	}
	channel := srv.channels[requestData.ChannelID]
	if channel == nil || !hasRecipient(channel, authUser) {
		return nil, &apiError{
			discordCode:    10003,
			err:            fmt.Errorf("unknown channel %q", requestData.ChannelID),
			httpStatusCode: http.StatusBadRequest,
		}
	}
	msg := &discord.Message{
		ID:        strconv.FormatInt(int64(srv.snowflake.Generate()), 10),
		ChannelID: channel.ID,
		Author:    authUser,
		Content:   requestData.Content,
		Timestamp: time.Now(),
		Type:      discord.MessageTypeDefault,
	}
	srv.messages[msg.ID] = msg
	eventPayload := newEventPayload("MESSAGE_CREATE", msg)
	for g := range srv.gateways {
		if g.authUser == nil {
			continue
		}
		for _, r := range channel.Recipients {
			if r == g.authUser {
				g.lockedPush(eventPayload)
				break
			}
		}
	}
	return newResponse(http.StatusCreated, msg)
}

func (srv *Server) createReaction(ctx context.Context, r *apiRequest) (*apiResponse, error) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	authUser := srv.lockedRequestUser(r)
	if authUser == nil {
		return nil, errUnauthorized
	}
	channelID := r.pathVars["channel.id"]
	channel := srv.channels[channelID]
	if channel == nil || !hasRecipient(channel, authUser) {
		return nil, &apiError{
			discordCode:    10003,
			err:            fmt.Errorf("unknown channel %q", channelID),
			httpStatusCode: http.StatusNotFound,
		}
	}
	messageID := r.pathVars["message.id"]
	message := srv.messages[messageID]
	if message == nil || message.ChannelID != channelID {
		return nil, &apiError{
			discordCode:    10008,
			err:            fmt.Errorf("unknown message %q", channelID),
			httpStatusCode: http.StatusNotFound,
		}
	}
	reactions := srv.reactions[messageID]
	if reactions == nil {
		reactions = make(map[string]map[*discord.User]struct{})
		srv.reactions[messageID] = reactions
	}
	emoji := r.pathVars["emoji"]
	usersForEmoji := reactions[emoji]
	if usersForEmoji == nil {
		usersForEmoji = make(map[*discord.User]struct{})
		reactions[emoji] = usersForEmoji
	}
	usersForEmoji[authUser] = struct{}{}
	return &apiResponse{statusCode: http.StatusNoContent}, nil
}

func (srv *Server) lockedFormatMessageForUser(u *discord.User, msg *discord.Message) *discord.Message {
	msg2 := new(discord.Message)
	*msg2 = *msg
	msg2.Reactions = nil
	for emoji, users := range srv.reactions[msg.ID] {
		reaction := &discord.Reaction{
			Count: len(users),
			Emoji: &discord.Emoji{Name: emoji},
		}
		_, reaction.Me = users[u]
		msg2.Reactions = append(msg2.Reactions, reaction)
	}
	return msg2
}

func (srv *Server) lockedRequestUser(r *apiRequest) *discord.User {
	return srv.auths[discord.AuthHeader(r.header.Get(authorizationHeaderName))]
}

// ApplicationDevInfo represents the information a Discord developer obtains
// from the developer portal.
type ApplicationDevInfo struct {
	ID       string
	BotToken string
}

// RegisterBot creates a new application and bot user.
func (srv *Server) RegisterBot(username string) *ApplicationDevInfo {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if err := srv.lockedInit(); err != nil {
		panic(err)
	}
	app := new(ApplicationDevInfo)
	app.ID = strconv.FormatInt(int64(srv.snowflake.Generate()), 10)
	_, app.BotToken = srv.lockedRegisterUser(username, true)
	return app
}

// RegisterUser creates a new end-user account.
func (srv *Server) RegisterUser(username string) (bearerToken string) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if err := srv.lockedInit(); err != nil {
		panic(err)
	}
	_, bearerToken = srv.lockedRegisterUser(username, false)
	return bearerToken
}

func (srv *Server) lockedRegisterUser(username string, bot bool) (u *discord.User, token string) {
	u = &discord.User{
		ID:            strconv.FormatInt(int64(srv.snowflake.Generate()), 10),
		Username:      username,
		Discriminator: fmt.Sprintf("%04d", srv.rand.Intn(10000)),
		Bot:           bot,
	}
	tokenBits := make([]byte, 10)
	srv.rand.Read(tokenBits[:])
	token = hex.EncodeToString(tokenBits)
	srv.users[u.ID] = u
	var auth discord.AuthHeader
	if bot {
		auth = discord.BotAuthorization(token)
	} else {
		auth = discord.BearerAuthorization(token)
	}
	srv.auths[auth] = u
	return u, token
}

func hasRecipient(c *discord.Channel, u *discord.User) bool {
	for _, recipient := range c.Recipients {
		if recipient == u {
			return true
		}
	}
	return false
}
