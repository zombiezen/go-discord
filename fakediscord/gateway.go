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

package fakediscord

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/sync/errgroup"
	"zombiezen.com/go/discord"
	"zombiezen.com/go/log"
)

func (srv *Server) getGateway(ctx context.Context, r *apiRequest) (*apiResponse, error) {
	return newResponse(http.StatusOK, map[string]interface{}{
		"url": (&url.URL{
			Scheme: "ws",
			Host:   r.host,
			Path:   "/gateway",
		}).String(),
	})
}

// gateway is protected by srv.mu
type gateway struct {
	authUser *discord.User
	// queueNotify is non-nil when a websocket is listening,
	// and it is closed when queue has its first element.
	queueNotify chan struct{}
	queue       []*discord.GatewayPayload
}

func (g *gateway) lockedPush(payload *discord.GatewayPayload) {
	g.queue = append(g.queue, payload)
	if len(g.queue) == 1 && g.queueNotify != nil {
		close(g.queueNotify)
		g.queueNotify = nil
	}
}

func (srv *Server) handleGatewayWebsocket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()
	if query.Get("v") != "9" {
		http.NotFound(w, r)
		return
	}
	if query.Get("encoding") != "json" {
		http.NotFound(w, r)
		return
	}
	if query.Get("compress") != "" {
		http.NotFound(w, r)
		return
	}

	conn, err := new(websocket.Upgrader).Upgrade(w, r, nil)
	if err != nil {
		srv.mu.Lock()
		log.Logf(ctx, srv.logger, log.Error, "Gateway websocket: %v", err)
		srv.mu.Unlock()
		return
	}
	defer conn.Close()
	err = conn.WriteJSON(newGatewayPayload(10, map[string]interface{}{
		"heartbeat_interval": 45000,
	}))
	if err != nil {
		srv.mu.Lock()
		log.Logf(ctx, srv.logger, log.Error, "Gateway websocket: %v", err)
		srv.mu.Unlock()
		return
	}

	myGateway := new(gateway)
	srv.mu.Lock()
	srv.gateways[myGateway] = struct{}{}
	srv.mu.Unlock()
	defer func() {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		// TODO(someday): Keep around for resume.
		delete(srv.gateways, myGateway)
	}()

	grp, ctx := errgroup.WithContext(ctx)
	outbound := make(chan *discord.GatewayPayload)
	send := func(msg *discord.GatewayPayload) error {
		select {
		case outbound <- msg:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	grp.Go(func() error {
		// Read loop.
		var payload discord.GatewayPayload
		ctxDone := ctx.Done()
		for {
			select {
			case <-ctxDone:
				return fmt.Errorf("read gateway payload: %v", ctx.Err())
			default:
			}
			read := make(chan struct{})
			watchDone := make(chan struct{})
			go func() {
				close(watchDone)
				select {
				case <-read:
				case <-ctxDone:
					conn.SetReadDeadline(time.Now())
				}
			}()
			_, packet, err := conn.ReadMessage()
			close(read)
			<-watchDone
			if err != nil {
				return fmt.Errorf("read gateway payload: %v", err)
			}

			payload = discord.GatewayPayload{}
			if err := json.Unmarshal(packet, &payload); err != nil {
				return err
			}
			switch payload.OpCode {
			case 1: // heartbeat
				if err := send(&discord.GatewayPayload{OpCode: 11}); err != nil {
					return err
				}
			case 2: // identify
				var identify struct {
					Token string `json:"token"`
				}
				if err := json.Unmarshal(payload.Data, &identify); err != nil {
					return err
				}
				srv.mu.Lock()
				u := srv.auths[discord.BotAuthorization(identify.Token)]
				if u == nil {
					u = srv.auths[discord.BearerAuthorization(identify.Token)]
				}
				if u != nil {
					myGateway.authUser = u
				}
				ready := newEventPayload("READY", map[string]interface{}{
					"v":    9,
					"user": u,
				})
				srv.mu.Unlock()

				if u == nil {
					// Invalid token: send Invalid Session.
					if err := send(newGatewayPayload(9, false)); err != nil {
						return err
					}
					continue
				}
				if err := send(ready); err != nil {
					return err
				}
			default:
				return fmt.Errorf("client sent unknown op-code %d", payload.OpCode)
			}
		}
	})

	grp.Go(func() error {
		// Send event notifications.
		for {
			srv.mu.Lock()
			for len(myGateway.queue) == 0 {
				wait := make(chan struct{})
				myGateway.queueNotify = wait
				srv.mu.Unlock()
				select {
				case <-wait:
				case <-ctx.Done():
					return ctx.Err()
				}
				srv.mu.Lock()
			}
			queue := myGateway.queue
			myGateway.queue = nil
			srv.mu.Unlock()

			for _, payload := range queue {
				if err := send(payload); err != nil {
					return err
				}
			}
		}
	})

	grp.Go(func() error {
		// Serialize message sending from other goroutines.
		for {
			select {
			case msg := <-outbound:
				data, err := json.Marshal(msg)
				if err != nil {
					return err
				}
				err = conn.WriteMessage(websocket.TextMessage, data)
				if err != nil {
					return err
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})
	if err := grp.Wait(); err != nil {
		srv.mu.Lock()
		log.Logf(ctx, srv.logger, log.Warn, "Websocket shutdown: %v", err)
		srv.mu.Unlock()
	}
}

func newEventPayload(eventName string, data interface{}) *discord.GatewayPayload {
	payload := newGatewayPayload(0, data)
	payload.EventName = eventName
	return payload
}

func newGatewayPayload(opCode int, data interface{}) *discord.GatewayPayload {
	payload := &discord.GatewayPayload{
		OpCode: opCode,
	}
	var err error
	payload.Data, err = json.Marshal(data)
	if err != nil {
		panic(err)
	}
	return payload
}
