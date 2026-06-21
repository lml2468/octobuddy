package main

import (
	"context"
	"embed"
	"log"
	"os"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/lml2468/xclaw/desktop/internal/autostart"
	"github.com/lml2468/xclaw/desktop/internal/control"
	"github.com/lml2468/xclaw/desktop/internal/octocli"
)

//go:embed all:frontend/dist
var assets embed.FS

func init() {
	// Strongly-typed event the backend pushes for every control-bus envelope.
	application.RegisterEvent[control.Envelope](EventStream)
}

const consoleWindow = "console"

var (
	app     *application.App
	bridge  *XClawService
	preview bool
	baseURL = "/"
)

func main() {
	preview = os.Getenv("XCLAW_PREVIEW") != ""

	// All the desktop's per-bot data lives under ~/.xclaw/<id>/. If HOME is
	// unset (rare but real on misconfigured launchd / systemd units) every
	// `home, _ := os.UserHomeDir()` site below silently lands at "/.xclaw"
	// or even ".xclaw" relative to CWD — config writes, octo-cli installs,
	// secret reads all scatter to the wrong place. Fail loudly at startup
	// rather than corrupting on first use (round 13 arch #3).
	if _, err := os.UserHomeDir(); err != nil {
		log.Fatalf("xclaw: cannot resolve user home directory: %v", err)
	}

	services := []application.Service{}
	if !preview {
		bridge = NewXClawService()
		services = append(services, application.NewService(bridge))
		// Install the bundled octo-cli baseline into ~/.xclaw/bin before the
		// daemon (and its agent) spawn, so it's on the agent's PATH from turn one.
		if err := octocli.EnsureInstalled(); err != nil {
			log.Printf("xclaw: octo-cli install skipped: %v", err)
		}
	}

	app = application.New(application.Options{
		Name:        "XClaw",
		Description: "XClaw — agent gateway desktop",
		Services:    services,
		Assets:      application.AssetOptions{Handler: application.AssetFileServerFS(assets)},
		// One running copy; a second launch focuses the existing console.
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "app.xclaw.dev",
			OnSecondInstanceLaunch: func(application.SecondInstanceData) {
				openConsole()
			},
		},
		Mac: application.MacOptions{
			// Keep the app alive (menu-bar agent) when the console window closes.
			ApplicationShouldTerminateAfterLastWindowClosed: false,
			ActivationPolicy: application.ActivationPolicyRegular,
		},
	})

	if preview {
		baseURL = "/?preview=1"
		if os.Getenv("XCLAW_PREVIEW_EDITOR") != "" {
			baseURL += "&editor=1"
		}
		if os.Getenv("XCLAW_PREVIEW_EMPTY") != "" {
			baseURL += "&empty=1"
		}
		if t := os.Getenv("XCLAW_PREVIEW_THEME"); t != "" {
			baseURL += "&theme=" + t
		}
	}

	openConsole()
	setupSystemTray()

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}

// openConsole shows the console window, recreating it if it was closed.
func openConsole() {
	if app == nil {
		return
	}
	if w, ok := app.Window.GetByName(consoleWindow); ok {
		w.Show()
		w.Focus()
		return
	}
	w := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:      consoleWindow,
		Title:     "XClaw",
		Width:     1040,
		Height:    720,
		MinWidth:  820,
		MinHeight: 560,
		// Frameless + transparent window: the OS draws no chrome and the corners
		// outside the content's 4px CSS radius show through, so the app gets a
		// subtle ≤4px rounding (not the native ~10px, not dead-square). Custom
		// traffic lights live in-app.
		Frameless: true,
		Mac: application.MacWindow{
			Backdrop: application.MacBackdropNormal,
		},
		BackgroundType:   application.BackgroundTypeTransparent,
		BackgroundColour: application.NewRGBA(0, 0, 0, 0),
		URL:              baseURL,
	})
	w.Show()
	w.Focus()
}

// setupSystemTray adds the menu-bar octopus with quick actions + a status line.
func setupSystemTray() {
	tray := app.SystemTray.New()
	tray.SetTemplateIcon(xMarkTemplatePNG())
	tray.SetTooltip("XClaw")

	menu := app.NewMenu()
	menu.Add("Open Console").OnClick(func(*application.Context) { openConsole() })
	menu.Add("Settings…").OnClick(func(*application.Context) {
		openConsole()
		app.Event.Emit("xclaw:open-settings", map[string]string{"tab": "basic"})
	})
	menu.Add("Token Usage…").OnClick(func(*application.Context) {
		openConsole()
		app.Event.Emit("xclaw:open-usage")
	})
	menu.AddSeparator()
	menu.Add("Restart Core").OnClick(func(*application.Context) {
		if bridge != nil {
			go bridge.RestartCore()
		}
	})

	// Launch at Login: a per-user LaunchAgent plist under ~/Library/LaunchAgents.
	// macOS-only; on other platforms autostart.Enabled returns false and we skip
	// the row entirely (no dead checkbox in the tray on linux/windows).
	if autostart.Supported() {
		menu.AddSeparator()
		on, _ := autostart.Enabled()
		login := menu.AddCheckbox("Launch at Login", on)
		login.OnClick(func(*application.Context) {
			want := login.Checked()
			var err error
			if want {
				err = autostart.Enable()
			} else {
				err = autostart.Disable()
			}
			if err != nil {
				log.Printf("xclaw: launch-at-login %v failed: %v", want, err)
				// Re-read the on-disk truth — the click may have flipped the
				// checkbox optimistically before the operation failed.
				if real, rerr := autostart.Enabled(); rerr == nil {
					login.SetChecked(real)
				} else {
					login.SetChecked(!want)
				}
			}
		})
	}

	// octo-cli companion: show the installed version + a one-click upgrade.
	menu.AddSeparator()
	octoInfo := menu.Add(octoInfoLabel())
	octoInfo.SetEnabled(false)
	menu.Add("Update octo-cli").OnClick(func(*application.Context) {
		go func() {
			octoInfo.SetLabel("Updating octo-cli…")
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			ver, err := octocli.Upgrade(ctx)
			if err != nil {
				log.Printf("xclaw: octo-cli update failed: %v", err)
				octoInfo.SetLabel("octo-cli — update failed")
				return
			}
			log.Printf("xclaw: octo-cli updated to %s", ver)
			octoInfo.SetLabel("octo-cli " + ver)
		}()
	})

	menu.AddSeparator()
	menu.Add("Quit XClaw").OnClick(func(*application.Context) { app.Quit() })
	tray.SetMenu(menu)
}

// octoInfoLabel is the (disabled) tray row showing the installed octo-cli version.
func octoInfoLabel() string {
	if v := octocli.InstalledVersion(); v != "" {
		return "octo-cli " + v
	}
	return "octo-cli — not installed"
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}
