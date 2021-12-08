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

package discord_test

import (
	"context"
	"encoding/json"
	"fmt"

	"zombiezen.com/go/discord"
)

// A simple program that sends "Hello, World!" to a user.
func Example() {
	ctx := context.TODO()

	// Get authorization token. Details at
	// https://discord.com/developers/docs/reference#authentication
	auth := discord.BotAuthorization("xyzzy")

	// Create a client.
	client := discord.NewClient(auth, nil)

	// Get or create a DM channel for a particular user.
	// https://discord.com/developers/docs/resources/user#create-dm
	//
	// To find out your user ID, see
	// https://support.discord.com/hc/en-us/articles/206346498-Where-can-I-find-my-User-Server-Message-ID-
	userID := "123"
	dmChannel, err := client.CreateDM(ctx, userID)
	if err != nil {
		// ... handle error ...
	}

	// Send a message on a channel.
	// https://discord.com/developers/docs/resources/channel#create-message
	_, err = client.CreateMessage(ctx, &discord.CreateMessageParams{
		ChannelID: dmChannel.ID,
		Content:   "Hello, World!",
	})
	if err != nil {
		// ... handle error ...
	}
}

// Discord's gateway enables receiving real-time events.
// This example shows how to set up a basic event loop to process incoming messages.
func ExampleGateway() {
	ctx := context.TODO()

	// Get authorization token. Details at
	// https://discord.com/developers/docs/reference#authentication
	auth := discord.BotAuthorization("xyzzy")

	// Create a client.
	client := discord.NewClient(auth, nil)

	// Open a connection to the Discord gateway.
	gateway, err := client.OpenGateway(ctx)
	if err != nil {
		// ... handle error ...
	}
	defer func() {
		if err := gateway.Close(); err != nil {
			// ... handle error ...
		}
	}()

	// Start listening for events.
	for {
		event, err := gateway.Listen(ctx)
		if err != nil {
			// ... handle error ...
			break
		}

		switch event.EventName {
		case "MESSAGE_CREATE":
			// The event data is a discord.Message JSON object.
			// https://discord.com/developers/docs/topics/gateway#message-create
			msg := new(discord.Message)
			if err := json.Unmarshal(event.Data, msg); err != nil {
				// ... handle error ...
				continue
			}

			fmt.Printf("%v: %s\n", msg.User(), msg.Content)
		default:
			// ... others ...
		}
	}
}
