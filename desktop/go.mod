module github.com/lml2468/octobuddy/desktop

go 1.26.5

require (
	github.com/wailsapp/wails/v3 v3.0.0-alpha2.108
	github.com/zalando/go-keyring v0.2.8
)

require github.com/danieljoos/wincred v1.2.3 // indirect

require (
	github.com/adrg/xdg v0.5.3 // indirect
	github.com/coder/websocket v1.8.14 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/jchv/go-winloader v0.0.0-20250406163304-c1995be93bd1 // indirect
	github.com/lml2468/octobuddy/core v0.0.0-00010101000000-000000000000
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/wailsapp/wails/webview2 v1.0.27 // indirect
	golang.org/x/sys v0.46.0 // indirect
)

replace github.com/lml2468/octobuddy/core => ../core
