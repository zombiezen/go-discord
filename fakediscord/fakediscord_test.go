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
	"net"
	"net/http/httptest"
	"net/url"
	"testing"

	"zombiezen.com/go/discord"
	"zombiezen.com/go/log/testlog"
)

func startServer(tb testing.TB) (*Server, *discord.ClientOptions) {
	tb.Helper()
	srv := new(Server)
	srv.SetLogger(testlog.Logger{})
	hsrv := httptest.NewUnstartedServer(srv)
	hsrv.Config.BaseContext = func(l net.Listener) context.Context {
		return testlog.WithTB(context.Background(), tb)
	}
	hsrv.Start()
	tb.Cleanup(hsrv.Close)
	baseURL, err := url.Parse(hsrv.URL + "/api/v9")
	if err != nil {
		tb.Fatal(err)
	}
	return srv, &discord.ClientOptions{
		BaseURL:    baseURL,
		HTTPClient: hsrv.Client(),
	}
}

func TestRegisterUser(t *testing.T) {
	ctx := testlog.WithTB(context.Background(), t)
	srv, clientOpts := startServer(t)
	const username = "cortana"
	bearerToken := srv.RegisterUser(username)
	dc := discord.NewClient(discord.BearerAuthorization(bearerToken), clientOpts)

	info, err := dc.CurrentAuthorization(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.User.Bot {
		t.Error("User is registered as a bot")
	}
	t.Log("User:", info.User)
	if info.User.Username != username {
		t.Errorf("username = %q; want %q", info.User.Username, username)
	}
}

func TestRegisterBot(t *testing.T) {
	ctx := testlog.WithTB(context.Background(), t)
	srv, clientOpts := startServer(t)
	const username = "robot"
	devInfo := srv.RegisterBot(username)
	dc := discord.NewClient(discord.BotAuthorization(devInfo.BotToken), clientOpts)

	info, err := dc.CurrentAuthorization(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !info.User.Bot {
		t.Error("User is not registered as a bot")
	}
	t.Log("User:", info.User)
	if info.User.Username != username {
		t.Errorf("username = %q; want %q", info.User.Username, username)
	}
}

func TestDM(t *testing.T) {
	ctx := testlog.WithTB(context.Background(), t)
	srv, clientOpts := startServer(t)
	bearerToken1 := srv.RegisterUser("user1")
	dc1 := discord.NewClient(discord.BearerAuthorization(bearerToken1), clientOpts)
	bearerToken2 := srv.RegisterUser("user2")
	dc2 := discord.NewClient(discord.BearerAuthorization(bearerToken2), clientOpts)
	auth2Info, err := dc2.CurrentAuthorization(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gateway2, err := dc2.OpenGateway(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := gateway2.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := gateway2.WaitForReady(ctx); err != nil {
		t.Fatal(err)
	}
	listenCtx, cancelListen := context.WithCancel(ctx)
	createMessageEventChan := make(chan *discord.GatewayPayload)
	defer func() {
		cancelListen()
		for range createMessageEventChan {
		}
	}()
	go func() {
		defer close(createMessageEventChan)
		for {
			payload, err := gateway2.Listen(listenCtx)
			if err != nil {
				t.Error(err)
				return
			}
			if payload.EventName == "MESSAGE_CREATE" {
				select {
				case createMessageEventChan <- payload:
				case <-listenCtx.Done():
				}
				return
			}
		}
	}()

	channel, err := dc1.CreateDM(ctx, auth2Info.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if channel.ID == "" {
		t.Fatal("DM channel ID empty")
	}
	if gotChannel, err := dc1.GetChannel(ctx, channel.ID); err != nil {
		t.Fatal(err)
	} else {
		if gotChannel.ID != channel.ID {
			t.Errorf("fetched DM channel ID = %q; want %q", gotChannel.ID, channel.ID)
		}
		if want := discord.ChannelTypeDM; gotChannel.Type != want {
			t.Errorf("fetched DM channel type = %d; want %d", gotChannel.Type, want)
		}
	}

	const msgContent = "Hello, World!"
	msg, err := dc1.CreateMessage(ctx, &discord.CreateMessageParams{
		ChannelID: channel.ID,
		Content:   msgContent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.ID == "" {
		t.Error("Sent message ID empty")
	}
	ev, ok := <-createMessageEventChan
	if !ok {
		return
	}
	receivedMsg := new(discord.Message)
	if err := json.Unmarshal(ev.Data, receivedMsg); err != nil {
		t.Fatal(err)
	}
	if receivedMsg.ID != msg.ID {
		t.Errorf("received message ID = %q; want %q", receivedMsg.ID, msg.ID)
	}
	if receivedMsg.ChannelID != channel.ID {
		t.Errorf("received message channel ID = %q; want %q", receivedMsg.ChannelID, channel.ID)
	}
	if receivedMsg.Content != msgContent {
		t.Errorf("received message channel ID = %q; want %q", receivedMsg.Content, msgContent)
	}
}
