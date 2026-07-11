package discord

import "github.com/bwmarrin/discordgo"

func BriefCommand(globalScope bool) *discordgo.ApplicationCommand {
	command := &discordgo.ApplicationCommand{
		Type:        discordgo.ChatApplicationCommand,
		Name:        "brief",
		Description: "プログラミングニュースのブリーフィングを操作します",
		Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "now", Description: "今すぐニュースを取得して投稿します"},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "preview", Description: "候補を自分だけにプレビューします"},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "status", Description: "Botと最終実行の状態を表示します"},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "feeds", Description: "有効なフィードと取得状態を表示します"},
		},
	}
	// Discord only accepts the contexts field for globally scoped commands.
	// Guild commands are already restricted by their registration endpoint.
	if globalScope {
		contexts := []discordgo.InteractionContextType{discordgo.InteractionContextGuild}
		command.Contexts = &contexts
	}
	return command
}

func Subcommand(data discordgo.ApplicationCommandInteractionData) string {
	if len(data.Options) != 1 || data.Options[0] == nil || data.Options[0].Type != discordgo.ApplicationCommandOptionSubCommand {
		return ""
	}
	return data.Options[0].Name
}

func InteractionUserID(i *discordgo.InteractionCreate) string {
	if i == nil || i.Interaction == nil {
		return ""
	}
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

func InteractionIsAdmin(i *discordgo.InteractionCreate) bool {
	return i != nil && i.Member != nil && i.Member.Permissions&discordgo.PermissionAdministrator != 0
}
