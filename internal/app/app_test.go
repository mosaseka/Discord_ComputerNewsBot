package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/config"
)

func TestAuthorizationAllowlistTakesPrecedence(t *testing.T) {
	a := &App{config: config.Config{AllowedUserIDs: map[string]struct{}{"42": {}}}}
	allowed := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{GuildID: "1", Member: &discordgo.Member{User: &discordgo.User{ID: "42"}}}}
	adminNotListed := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{GuildID: "1", Member: &discordgo.Member{User: &discordgo.User{ID: "99"}, Permissions: discordgo.PermissionAdministrator}}}
	if !a.authorized(allowed) || a.authorized(adminNotListed) {
		t.Fatal("allowlist authorization did not take precedence")
	}
}

func TestFixtureApplicationDoesNotConstructHTTPClient(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Tokyo")
	cfg := config.Config{
		Location: loc, DatabasePath: filepath.Join(t.TempDir(), "bot.db"),
		Mode: config.Mode{Once: true, DryRun: true, Fixture: "fixtures"},
	}
	a, err := New(context.Background(), cfg, nil, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer a.Close()
	if a.http != nil {
		t.Fatal("fixture application constructed an HTTP client")
	}
}

func TestAuthorizationRequiresAdminWithoutAllowlist(t *testing.T) {
	a := &App{config: config.Config{AllowedUserIDs: map[string]struct{}{}}}
	admin := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{GuildID: "1", Member: &discordgo.Member{User: &discordgo.User{ID: "99"}, Permissions: discordgo.PermissionAdministrator}}}
	dm := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{User: &discordgo.User{ID: "99"}}}
	if !a.authorized(admin) || a.authorized(dm) {
		t.Fatal("administrator/DM authorization is incorrect")
	}
}

func TestShutdownWaitsForTrackedWorkAndRejectsNewWork(t *testing.T) {
	a := &App{}
	if !a.beginWork() {
		t.Fatal("beginWork() rejected work before shutdown")
	}
	closed := make(chan error, 1)
	go func() { closed <- a.Close() }()

	deadline := time.Now().Add(time.Second)
	for {
		a.lifecycleMu.Lock()
		closing := a.closing
		a.lifecycleMu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shutdown did not enter closing state")
		}
		time.Sleep(time.Millisecond)
	}
	if a.beginWork() {
		a.wg.Done()
		t.Fatal("beginWork() accepted work during shutdown")
	}
	select {
	case err := <-closed:
		t.Fatalf("Close() returned before tracked work completed: %v", err)
	default:
	}
	a.wg.Done()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close() did not return after tracked work completed")
	}
}
