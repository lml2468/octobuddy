package main

import (
	"context"
	"embed"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/lml2468/octobuddy/desktop/internal/autostart"
	"github.com/lml2468/octobuddy/desktop/internal/control"
	"github.com/lml2468/octobuddy/desktop/internal/logfile"
	"github.com/lml2468/octobuddy/desktop/internal/octocli"
	"github.com/lml2468/octobuddy/desktop/internal/windowstate"
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
	bridge  *OctoBuddyService
	preview bool
	baseURL = "/"
	// logPath is where the combined desktop+daemon log lives — set during
	// main() once the home dir is verified and consumed by the tray menu's
	// "查看日志" action so it shows the user the same file we're writing to.
	// Empty when OCTOBUDDY_PREVIEW is on (preview mode skips persistent logging
	// since there's no daemon to surface and the dev console is right there).
	logPath string
)

func main() {
	preview = os.Getenv("OCTOBUDDY_PREVIEW") != ""

	// All the desktop's per-bot data lives under ~/.octobuddy/<id>/. If HOME is
	// unset (rare but real on misconfigured launchd / systemd units) every
	// `home, _ := os.UserHomeDir` site below silently lands at "/.octobuddy"
	// or even ".octobuddy" relative to CWD — config writes, octo-cli installs,
	// secret reads all scatter to the wrong place. Fail loudly at startup
	// rather than corrupting on first use.
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("octobuddy: cannot resolve user home directory: %v", err)
	}

	// Persistent logging: tee log.Print (our own log lines) and the daemon's
	// stdout+stderr to ~/.octobuddy.logs/octobuddy.log, rotated at 5 MiB. Without
	// this, the only way an end user (or you, helping them remotely) can see
	// "[gateway] terminal agent error: ..." or "[selfcheck] auth=UNSET" is
	// to relaunch from a terminal — which they will not. log file path is
	// also exposed via the tray menu's "查看日志" action.
	var logSink io.Writer = os.Stderr
	if !preview {
		w, err := logfile.New(filepath.Join(home, ".octobuddy", "logs"), "octobuddy.log", 5<<20)
		if err != nil {
			log.Printf("octobuddy: persistent log unavailable (%v) — logging to stderr only", err)
		} else {
			logPath = w.Path()
			logSink = w.Tee(os.Stderr)
			log.SetOutput(logSink)
		}
	}

	services := []application.Service{}
	if !preview {
		bridge = NewOctoBuddyService(logSink)
		services = append(services, application.NewService(bridge))
		// Install the bundled octo-cli baseline into ~/.octobuddy/bin before the
		// daemon (and its agent) spawn, so it's on the agent's PATH from turn one.
		if err := octocli.EnsureInstalled(); err != nil {
			log.Printf("octobuddy: octo-cli install skipped: %v", err)
		}
	}

	app = application.New(application.Options{
		Name:        "OctoBuddy",
		Description: "OctoBuddy — agent gateway desktop",
		Services:    services,
		Assets:      application.AssetOptions{Handler: application.AssetFileServerFS(assets)},
		// One running copy; a second launch focuses the existing console.
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "com.mlt.octobuddy.desktop",
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
		if os.Getenv("OCTOBUDDY_PREVIEW_EDITOR") != "" {
			baseURL += "&editor=1"
		}
		if os.Getenv("OCTOBUDDY_PREVIEW_EMPTY") != "" {
			baseURL += "&empty=1"
		}
		if t := os.Getenv("OCTOBUDDY_PREVIEW_THEME"); t != "" {
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
// Restores the last saved position+size when window.json exists, so
// reopening the console after a quit (or after closing+reopening the
// console window itself) lands in the same place users last left it —
// macOS HIG expectation, mild affordance on Win/Linux.
func openConsole() {
	if app == nil {
		return
	}
	if w, ok := app.Window.GetByName(consoleWindow); ok {
		w.Show()
		w.Focus()
		return
	}
	opts := application.WebviewWindowOptions{
		Name:      consoleWindow,
		Title:     "OctoBuddy",
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
	}
	// Restore saved bounds when present + sane (positive size; Wails clamps
	// to MinWidth/MinHeight so we don't need to). Position 0,0 is the
	// natural default; a saved 0,0 simply re-opens at the top-left, which
	// is fine and indistinguishable from a never-saved state.
	saved, err := windowstate.Load()
	if err != nil {
		log.Printf("octobuddy: window state load failed (using defaults): %v", err)
	}
	if !saved.IsZero() {
		if saved.Width > 0 {
			opts.Width = saved.Width
		}
		if saved.Height > 0 {
			opts.Height = saved.Height
		}
		opts.X = saved.X
		opts.Y = saved.Y
	}
	w := app.Window.NewWithOptions(opts)
	// Persist bounds on close. The window-closing event fires before the
	// window is destroyed, so Position()/Size() still return valid values.
	// Save is best-effort: a failed write is logged but doesn't block the
	// close.
	w.RegisterHook(events.Common.WindowClosing, func(*application.WindowEvent) {
		x, y := w.Position()
		width, height := w.Size()
		if err := windowstate.Save(windowstate.State{X: x, Y: y, Width: width, Height: height}); err != nil {
			log.Printf("octobuddy: window state save failed: %v", err)
		}
	})
	w.Show()
	w.Focus()
}

// setupSystemTray adds the menu-bar octopus with quick actions + a status line.
func setupSystemTray() {
	tray := app.SystemTray.New()
	tray.SetTemplateIcon(xMarkTemplatePNG())
	tray.SetTooltip("OctoBuddy")

	menu := app.NewMenu()
	menu.Add("Open Console").OnClick(func(*application.Context) { openConsole() })
	menu.Add("Settings…").OnClick(func(*application.Context) {
		openConsole()
		app.Event.Emit("octobuddy:open-settings", map[string]string{"tab": "basic"})
	})
	menu.Add("Token Usage…").OnClick(func(*application.Context) {
		openConsole()
		app.Event.Emit("octobuddy:open-usage")
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
				log.Printf("octobuddy: launch-at-login %v failed: %v", want, err)
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
				log.Printf("octobuddy: octo-cli update failed: %v", err)
				octoInfo.SetLabel("octo-cli — update failed")
				return
			}
			log.Printf("octobuddy: octo-cli updated to %s", ver)
			octoInfo.SetLabel("octo-cli " + ver)
		}()
	})

	// Diagnostics: open the persistent log file. The label says "查看日志"
	// because that's where users (and you, when helping a user remotely) go
	// first when "出错了，请稍后重试" shows up — the line containing the real
	// reason ([gateway] terminal agent error / [selfcheck] auth=UNSET / ...)
	// lives there and only there once the app is launched normally. Disabled
	// in preview mode where there's no log file to point at.
	menu.AddSeparator()
	logItem := menu.Add("查看日志")
	if logPath == "" {
		logItem.SetEnabled(false)
	} else {
		logItem.OnClick(func(*application.Context) { openLogInConsole(logPath) })
	}

	menu.AddSeparator()
	menu.Add("Quit OctoBuddy").OnClick(func(*application.Context) { app.Quit() })
	tray.SetMenu(menu)
}

// openLogInConsole reveals the persistent log file in macOS Console.app on
// darwin (so users can scroll, search, and tail live) and falls back to the
// platform's default opener on linux/windows. Errors get logged — which means
// they land in the same file the user just tried to open, which is fine: the
// next time they manage to open it they'll see WHY the prior attempt failed.
func openLogInConsole(path string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", "-a", "Console", path)
	case "windows":
		cmd = exec.Command("cmd", "/C", "start", "", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("octobuddy: open log %q failed: %v", path, err)
	}
}

// octoInfoLabel is the (disabled) tray row showing the installed octo-cli version.
func octoInfoLabel() string {
	if v := octocli.InstalledVersion(); v != "" {
		return "octo-cli " + v
	}
	return "octo-cli — not installed"
}
