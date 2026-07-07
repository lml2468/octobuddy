package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/lml2468/octobuddy/core/config"
	"github.com/lml2468/octobuddy/core/control"
	"github.com/lml2468/octobuddy/core/control/wire"
	"github.com/lml2468/octobuddy/core/im/octo"
	"github.com/lml2468/octobuddy/core/router"
)

// TestSecretInjectHandler verifies the multi-bot secret.inject command routes a
// secret into the addressed bot's in-memory store (and errors on unknown
// bot/kind). It only touches the secrets field, so gateway/store can be nil.
func TestSecretInjectHandler(t *testing.T) {
	reg := newBotRegistry(nil)
	sec := &secretStore{}
	bot := &botRuntime{cfg: config.Resolved{BotID: "b1"}, secrets: sec}
	bot.target = &botTarget{id: "b1", secrets: sec}
	reg.add(bot)
	h := makeMultiBotHandler(context.Background(), reg, time.Now())

	inject := func(b control.SecretInjectBody) (any, error) {
		raw, _ := json.Marshal(b)
		return h("secret.inject", raw)
	}

	if _, err := inject(control.SecretInjectBody{BotID: "b1", Kind: wire.SecretKindOcto, Value: "bf_new"}); err != nil {
		t.Fatalf("inject octo: %v", err)
	}
	if bot.secrets.OctoToken() != "bf_new" {
		t.Fatalf("octo token not stored: %q", bot.secrets.OctoToken())
	}

	if _, err := inject(control.SecretInjectBody{BotID: "b1", Kind: wire.SecretKindGateway, Value: "sk_g"}); err != nil {
		t.Fatalf("inject gateway: %v", err)
	}
	if bot.secrets.GatewayToken() != "sk_g" {
		t.Fatalf("gateway token not stored: %q", bot.secrets.GatewayToken())
	}

	if _, err := inject(control.SecretInjectBody{BotID: "nope", Kind: wire.SecretKindOcto, Value: "x"}); err == nil {
		t.Fatal("unknown bot should error")
	}
	if _, err := inject(control.SecretInjectBody{BotID: "b1", Kind: "bogus", Value: "x"}); err == nil {
		t.Fatal("unknown kind should error")
	}

	exerciseBotAssemblyHelpers(t)
}

func exerciseBotAssemblyHelpers(t *testing.T) {
	t.Helper()

	base := t.TempDir()
	cfg := config.Resolved{
		BotID:          "b2",
		DataDir:        filepath.Join(base, "data"),
		AgentConfigDir: filepath.Join(base, "claude"),
	}
	if err := prepareBotDirs(cfg); err != nil {
		t.Fatalf("prepareBotDirs: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	rt := router.New(router.Config{})
	startRouterReaper(ctx, rt, nil)
	cancel()

	connector := octo.NewConnector(octo.NewRESTClient("http://unused", func() string { return "" }))
	target := &botTarget{id: "b2"}
	drainBotRuntime(nil, connector, target)
	startBotCron(nil, "b2")

	cronOff := false
	cfg.Agent.Cron = &cronOff
	_ = newBotCronManager(context.Background(), cfg, connector, nil, target)
	registerBotRuntime(&botRuntime{cfg: cfg, connector: connector}, newBotRegistry(nil), nil)
}

// TestMCPCheckHandler verifies mcp.check routes to the addressed bot's hook
// (returning its health), reports "not configured" when no hook is wired, and
// errors on an unknown bot. The hook is faked — the real probe is covered by
// agent.TestProbeMCPReportsHealth.
func TestMCPCheckHandler(t *testing.T) {
	reg := newBotRegistry(nil)
	withHook := &botRuntime{cfg: config.Resolved{BotID: "hooked"}}
	withHook.target = &botTarget{id: "hooked", mcpCheck: func(context.Context) (control.MCPCheckResponse, error) {
		return control.MCPCheckResponse{Configured: true, Servers: []control.MCPServerHealth{{Name: "echo", Status: "connected", Tools: []string{"mcp__echo__ping"}}}}, nil
	}}
	noHook := &botRuntime{cfg: config.Resolved{BotID: "bare"}}
	noHook.target = &botTarget{id: "bare"}
	reg.add(withHook)
	reg.add(noHook)
	h := makeMultiBotHandler(context.Background(), reg, time.Now())

	check := func(botID string) (control.MCPCheckResponse, error) {
		raw, _ := json.Marshal(control.MCPCheckBody{BotID: botID})
		out, err := h("mcp.check", raw)
		if err != nil {
			return control.MCPCheckResponse{}, err
		}
		return out.(control.MCPCheckResponse), nil
	}

	res, err := check("hooked")
	if err != nil {
		t.Fatalf("check hooked: %v", err)
	}
	if !res.Configured || res.BotID != "hooked" || len(res.Servers) != 1 || res.Servers[0].Status != "connected" {
		t.Fatalf("hooked result wrong: %+v", res)
	}

	bare, err := check("bare")
	if err != nil {
		t.Fatalf("check bare: %v", err)
	}
	if bare.Configured || bare.BotID != "bare" {
		t.Fatalf("bare bot must report not-configured: %+v", bare)
	}

	if _, err := check("nope"); err == nil {
		t.Fatal("unknown bot should error")
	}
}
