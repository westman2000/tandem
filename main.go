package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"syscall"
	"tandem/internal/app"
)

//go:embed web/dist
var assets embed.FS

func main() {
	address := flag.String("listen", "127.0.0.1:4317", "Local listen address")
	data := flag.String("data", "", "Machine settings and legacy data (default: existing .tandem or user config/Tandem)")
	uiDir := flag.String("ui", "", "Development: serve UI from this directory instead of embedded assets")
	flag.Parse()
	if *data == "" {
		path, err := app.DefaultDataDirectory()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		*data = path
	}
	ui, _ := fs.Sub(assets, "web/dist")
	if *uiDir != "" {
		ui = os.DirFS(*uiDir)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := app.Serve(ctx, *address, *data, ui); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
