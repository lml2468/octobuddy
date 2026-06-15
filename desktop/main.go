package main

import (
	"embed"
	"log"
	"os"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/lml2468/xclaw/desktop/internal/control"
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

	services := []application.Service{}
	if !preview {
		bridge = NewXClawService()
		services = append(services, application.NewService(bridge))
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
		Width:     1200,
		Height:    780,
		MinWidth:  900,
		MinHeight: 600,
		Mac: application.MacWindow{
			InvisibleTitleBarHeight: 36,
			Backdrop:                application.MacBackdropNormal, // opaque paper — the watercolor canvas, not glass
			TitleBar:                application.MacTitleBarHiddenInset,
		},
		BackgroundColour: application.NewRGB(245, 239, 228), // paper (light); the canvas paints the rest
		URL:              baseURL,
	})
	w.Show()
	w.Focus()
}

// setupSystemTray adds the menu-bar octopus with quick actions + a status line.
func setupSystemTray() {
	tray := app.SystemTray.New()
	tray.SetTemplateIcon(octopusTemplatePNG())
	tray.SetTooltip("XClaw")

	menu := app.NewMenu()
	menu.Add("Open Console").OnClick(func(*application.Context) { openConsole() })
	menu.Add("Edit Bots…").OnClick(func(*application.Context) {
		openConsole()
		app.Event.Emit("xclaw:open-editor")
	})
	menu.AddSeparator()
	menu.Add("Restart Core").OnClick(func(*application.Context) {
		if bridge != nil {
			go bridge.RestartCore()
		}
	})
	menu.AddSeparator()
	menu.Add("Quit XClaw").OnClick(func(*application.Context) { app.Quit() })
	tray.SetMenu(menu)
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}
