# zombiezen Go Client for Discord

`zombiezen.com/go/discord` is a WIP Go library
for interacting with the [Discord API][].
It differs from [DiscordGo][] by providing a [`Context`][]-aware API.

[Discord API]: https://discord.com/developers/docs/intro
[DiscordGo]: https://github.com/bwmarrin/discordgo
[`Context`]: https://pkg.go.dev/context

## Installation

```shell
go get zombiezen.com/go/discord
```

## Basic Usage

```go
auth := discord.BotAuthorization("xyzzy")
client := discord.NewClient(auth, nil)
dmChannel, err := client.CreateDM(ctx, userID)
if err != nil {
  return err
}
_, err = client.CreateMessage(ctx, &discord.CreateMessageParams{
  ChannelID: dmChannel.ID,
  Content:   "Hello, World!",
})
if err != nil {
  return err
}
```

See pkg.go.dev for
[more examples](https://pkg.go.dev/zombiezen.com/go/discord#pkg-examples).

## Contributing

We'd love to accept your patches and contributions to this project.
See [CONTRIBUTING.md](CONTRIBUTING.md) for more details.

## Links

- [Discord Developer Documentation](https://discord.com/developers/docs/intro)
- [Release Notes](https://github.com/zombiezen/go-discord/blob/main/CHANGELOG.md)

## License

[Apache 2.0](LICENSE)
