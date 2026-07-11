package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/brief"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/config"
	discordout "github.com/mosaseka/Discord_ComputerNewsBot/internal/discord"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/httpclient"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/scheduler"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/storage"
)

const runTimeout = 5 * time.Minute

type App struct {
	config      config.Config
	feeds       []config.Feed
	logger      *slog.Logger
	store       *storage.Store
	http        *httpclient.Client
	discord     *discordout.Client
	brief       *brief.Service
	coordinator scheduler.Coordinator
	scheduler   *scheduler.Scheduler
	startedAt   time.Time
	ctx         context.Context
	wg          sync.WaitGroup
	lifecycleMu sync.Mutex
	closing     bool
	closeOnce   sync.Once
	closeErr    error
}

func New(ctx context.Context, cfg config.Config, feeds []config.Feed, logger *slog.Logger) (*App, error) {
	if logger == nil {
		logger = slog.Default()
	}
	store, err := storage.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	var httpClient *httpclient.Client
	if cfg.Mode.Fixture == "" {
		httpConfig := httpclient.DefaultConfig()
		httpConfig.UserAgent = cfg.HTTPUserAgent
		httpConfig.ArticleMaxBytes = cfg.MaxArticleBytes
		httpClient, err = httpclient.New(httpConfig, nil)
		if err != nil {
			store.Close()
			return nil, err
		}
	}
	application := &App{
		config: cfg, feeds: feeds, logger: logger, store: store, http: httpClient,
		startedAt: time.Now(), ctx: ctx,
	}
	if !cfg.Mode.DryRun {
		application.discord, err = discordout.NewClient(cfg.DiscordBotToken)
		if err != nil {
			store.Close()
			return nil, err
		}
	}
	var publisher discordout.Publisher
	if application.discord != nil {
		publisher = application.discord
	}
	application.brief = &brief.Service{
		Config: cfg, Feeds: feeds, Store: store, HTTP: httpClient, Publisher: publisher, Logger: logger,
	}
	return application, nil
}

func (a *App) RunOnce(ctx context.Context) (brief.Result, error) {
	runCtx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	runType := brief.RunOnce
	if a.config.Mode.DryRun {
		runType = brief.RunDry
	}
	request := brief.Request{
		Type: runType, ChannelID: a.config.DiscordChannelID, DryRun: a.config.Mode.DryRun,
		FixtureDir: a.config.Mode.Fixture,
	}
	var result brief.Result
	err := a.coordinator.TryRun(runCtx, func(runCtx context.Context) error {
		var runErr error
		result, runErr = a.brief.Run(runCtx, request)
		return runErr
	})
	return result, err
}

func (a *App) RunNormal(ctx context.Context) error {
	if a.discord == nil {
		return errors.New("Discord is not configured")
	}
	a.ctx = ctx
	a.discord.Session.AddHandler(a.handleInteraction)
	if err := a.reconcileBriefCommand(ctx); err != nil {
		return err
	}
	if err := a.discord.Session.Open(); err != nil {
		return fmt.Errorf("open Discord gateway: %w", err)
	}
	a.logger.Info("Discord gateway connected", "guild_command", a.config.DiscordGuildID != "")

	job, err := scheduler.New(ctx, a.config.BriefCron, a.config.Location, func(parent context.Context) {
		runCtx, cancel := context.WithTimeout(parent, runTimeout)
		defer cancel()
		err := a.coordinator.TryRun(runCtx, func(active context.Context) error {
			result, runErr := a.brief.Run(active, brief.Request{Type: brief.RunScheduled, ChannelID: a.config.DiscordChannelID})
			if runErr == nil {
				a.logger.Info("scheduled briefing completed", "fetched", result.FetchedCount, "selected", result.SelectedCount, "posted", result.PostedCount, "outcome", result.Outcome)
			}
			return runErr
		})
		if err != nil && !errors.Is(err, scheduler.ErrBusy) && !errors.Is(err, context.Canceled) {
			a.logger.Error("scheduled briefing failed", "error", err)
		}
	})
	if err != nil {
		a.discord.Session.Close()
		return err
	}
	a.scheduler = job
	a.scheduler.Start()
	if next, nextErr := a.scheduler.NextRun(); nextErr == nil {
		a.logger.Info("scheduler started", "next_run", next.In(a.config.Location))
	}

	<-ctx.Done()
	return a.shutdown()
}

func (a *App) Close() error { return a.shutdown() }

func (a *App) shutdown() error {
	a.closeOnce.Do(func() {
		var joined error
		if a.scheduler != nil {
			joined = errors.Join(joined, a.scheduler.Shutdown())
		}
		a.lifecycleMu.Lock()
		a.closing = true
		a.lifecycleMu.Unlock()
		if a.discord != nil && a.discord.Session != nil {
			joined = errors.Join(joined, a.discord.Session.Close())
		}
		a.wg.Wait()
		if a.http != nil {
			a.http.CloseIdleConnections()
		}
		if a.store != nil {
			joined = errors.Join(joined, a.store.Close())
		}
		a.closeErr = joined
	})
	return a.closeErr
}

func (a *App) reconcileBriefCommand(ctx context.Context) error {
	commands, err := a.discord.Session.ApplicationCommands(a.config.DiscordApplicationID, a.config.DiscordGuildID, discordgo.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("list Discord commands: %w", err)
	}
	desired := discordout.BriefCommand(a.config.DiscordGuildID == "")
	for _, existing := range commands {
		if existing.Name == desired.Name && existing.Type == desired.Type {
			_, err := a.discord.Session.ApplicationCommandEdit(a.config.DiscordApplicationID, a.config.DiscordGuildID, existing.ID, desired, discordgo.WithContext(ctx))
			if err != nil {
				return fmt.Errorf("update /brief command: %w", err)
			}
			return nil
		}
	}
	if _, err := a.discord.Session.ApplicationCommandCreate(a.config.DiscordApplicationID, a.config.DiscordGuildID, desired, discordgo.WithContext(ctx)); err != nil {
		return fmt.Errorf("create /brief command: %w", err)
	}
	return nil
}

func (a *App) handleInteraction(session *discordgo.Session, event *discordgo.InteractionCreate) {
	if !a.beginInteraction() {
		return
	}
	defer a.wg.Done()
	if event == nil || event.Type != discordgo.InteractionApplicationCommand {
		return
	}
	data := event.ApplicationCommandData()
	if data.Name != "brief" {
		return
	}
	subcommand := discordout.Subcommand(data)
	if subcommand == "" {
		a.respondEphemeral(session, event, "不正なサブコマンドです。")
		return
	}
	needsManualPermission := subcommand == "now" || subcommand == "preview" || !a.config.CommandStatusPublic
	if needsManualPermission && !a.authorized(event) {
		a.respondEphemeral(session, event, "このコマンドを実行する権限がありません。")
		return
	}
	switch subcommand {
	case "now", "preview":
		if err := session.InteractionRespond(event.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
		}, discordgo.WithContext(a.ctx)); err != nil {
			a.logger.Error("defer interaction response", "subcommand", subcommand, "error", err)
			return
		}
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.runInteraction(session, event, subcommand)
		}()
	case "status":
		a.respondEphemeral(session, event, a.statusText(a.ctx))
	case "feeds":
		a.respondEphemeral(session, event, a.feedsText(a.ctx))
	}
}

func (a *App) beginInteraction() bool {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	if a.closing {
		return false
	}
	a.wg.Add(1)
	return true
}

func (a *App) authorized(event *discordgo.InteractionCreate) bool {
	if event == nil || event.GuildID == "" {
		return false
	}
	userID := discordout.InteractionUserID(event)
	if len(a.config.AllowedUserIDs) > 0 {
		return a.config.IsAllowedUser(userID)
	}
	return discordout.InteractionIsAdmin(event)
}

func (a *App) runInteraction(session *discordgo.Session, event *discordgo.InteractionCreate, subcommand string) {
	ctx, cancel := context.WithTimeout(a.ctx, runTimeout)
	defer cancel()
	request := brief.Request{Type: brief.RunManual, ChannelID: a.config.DiscordChannelID}
	if subcommand == "preview" {
		request.Type = brief.RunPreview
		request.DryRun = true
	}
	var result brief.Result
	err := a.coordinator.TryRun(ctx, func(active context.Context) error {
		var runErr error
		result, runErr = a.brief.Run(active, request)
		return runErr
	})
	if err != nil {
		message := "実行に失敗しました: " + err.Error()
		if errors.Is(err, scheduler.ErrBusy) {
			message = "別のブリーフィングを実行中です。完了後に再試行してください。"
		}
		a.editInteraction(session, event, discordout.Truncate(message, discordout.MaxMessageContent), nil)
		return
	}
	if subcommand == "preview" {
		if len(result.Brief.Items) == 0 {
			a.editInteraction(session, event, "対象期間に投稿可能な記事はありませんでした。", nil)
			return
		}
		embeds := discordout.BuildEmbeds(result.Brief)
		batches, packErr := discordout.PackEmbeds(embeds)
		if packErr == nil && len(batches) == 1 {
			a.editInteraction(session, event, "", batches[0])
			return
		}
		var compact strings.Builder
		for i, item := range result.Brief.Items {
			fmt.Fprintf(&compact, "%d. %s\n%s\n", i+1, item.Title, item.URL)
		}
		a.editInteraction(session, event, discordout.Truncate(compact.String(), discordout.MaxMessageContent), nil)
		return
	}
	message := fmt.Sprintf("投稿が完了しました。取得 %d件、選定 %d件、投稿 %d件。", result.FetchedCount, result.SelectedCount, result.PostedCount)
	a.editInteraction(session, event, message, nil)
}

func (a *App) respondEphemeral(session *discordgo.Session, event *discordgo.InteractionCreate, content string) {
	content = discordout.Truncate(content, discordout.MaxMessageContent)
	err := session.InteractionRespond(event.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content, Flags: discordgo.MessageFlagsEphemeral,
			AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
		},
	}, discordgo.WithContext(a.ctx))
	if err != nil {
		a.logger.Error("respond to interaction", "error", err)
	}
}

func (a *App) editInteraction(session *discordgo.Session, event *discordgo.InteractionCreate, content string, embeds []*discordgo.MessageEmbed) {
	content = discordout.Truncate(content, discordout.MaxMessageContent)
	edit := &discordgo.WebhookEdit{AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}}
	edit.Content = &content
	if embeds != nil {
		edit.Embeds = &embeds
	}
	if _, err := session.InteractionResponseEdit(event.Interaction, edit, discordgo.WithContext(a.ctx)); err != nil {
		a.logger.Error("edit interaction response", "error", err)
	}
}

func (a *App) statusText(ctx context.Context) string {
	status, err := a.store.GetStatus(ctx)
	if err != nil {
		return "状態の取得に失敗しました: " + err.Error()
	}
	next := "未設定"
	if a.scheduler != nil {
		if value, nextErr := a.scheduler.NextRun(); nextErr == nil {
			next = value.In(a.config.Location).Format(time.RFC3339)
		}
	}
	lastRun := formatOptionalTime(status.LastRunAt, a.config.Location)
	lastSuccess := formatOptionalTime(status.LastSuccessAt, a.config.Location)
	lastError := status.LastError
	if lastError == "" {
		lastError = "なし"
	}
	unknown := "なし"
	if status.UnknownBatchCount > 0 {
		unknown = fmt.Sprintf("%d件（最新: %s）", status.UnknownBatchCount, status.LastUnknownError)
	}
	text := fmt.Sprintf("稼働時間: %s\n次回実行時刻: %s\n最終実行時刻: %s\n最終成功時刻: %s\n最終失敗内容: %s\n送信成否不明batch: %s\n最後に取得した記事数: %d\n最後に選定した記事数: %d\n最後に投稿した記事数: %d\nDBパス: %s\n有効なフィード数: %d",
		time.Since(a.startedAt).Round(time.Second), next, lastRun, lastSuccess, lastError,
		unknown, status.FetchedCount, status.SelectedCount, status.PostedCount, status.DatabasePath, enabledFeedCount(a.feeds))
	return discordout.Truncate(text, discordout.MaxMessageContent)
}

func (a *App) feedsText(ctx context.Context) string {
	statuses, err := a.store.ListFeedStatuses(ctx)
	if err != nil {
		return "フィード状態の取得に失敗しました: " + err.Error()
	}
	byName := make(map[string]storage.FeedStatus, len(statuses))
	for _, status := range statuses {
		byName[status.Name] = status
	}
	feeds := append([]config.Feed(nil), a.feeds...)
	sort.Slice(feeds, func(i, j int) bool { return feeds[i].Name < feeds[j].Name })
	var out strings.Builder
	for _, configured := range feeds {
		if !configured.Enabled {
			continue
		}
		status, ok := byName[configured.Name]
		if !ok {
			fmt.Fprintf(&out, "・%s — 未取得\n", configured.Name)
			continue
		}
		marker := "成功"
		if status.Status == "failure" {
			marker = "失敗"
		}
		fmt.Fprintf(&out, "・%s — %s / %d件 / %s\n", configured.Name, marker, status.ItemCount, status.CheckedAt.In(a.config.Location).Format("01-02 15:04"))
	}
	return discordout.Truncate(out.String(), discordout.MaxMessageContent)
}

func enabledFeedCount(feeds []config.Feed) int {
	count := 0
	for _, feed := range feeds {
		if feed.Enabled {
			count++
		}
	}
	return count
}

func formatOptionalTime(value *time.Time, location *time.Location) string {
	if value == nil {
		return "未実行"
	}
	return value.In(location).Format(time.RFC3339)
}
