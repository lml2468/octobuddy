package main

import _ "embed"

// octoMenuBarPNG is the Octo brand icon shown in the macOS menu bar. Unlike a
// template icon (which macOS tints monochrome to match the bar), this is set
// via SystemTray.SetIcon so it renders in its actual brand colors.
//
//go:embed trayicon_octo.png
var octoMenuBarPNG []byte
