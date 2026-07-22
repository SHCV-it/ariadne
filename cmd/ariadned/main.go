// Command ariadned is the completion daemon.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"

	"ariadne/internal/daemon"
)

func main() {
	cfg := daemon.DefaultConfig()
	flag.StringVar(&cfg.SocketPath, "socket", cfg.SocketPath, "unix socket path")
	flag.StringVar(&cfg.DataDir, "data", cfg.DataDir, "data directory")
	deny := flag.String("deny", "", "comma-separated regexes of commands never to record")
	flag.BoolVar(&cfg.Verbose, "v", false, "verbose logging")
	flag.Parse()

	if *deny != "" {
		for _, p := range strings.Split(*deny, ",") {
			re, err := regexp.Compile(strings.TrimSpace(p))
			if err != nil {
				log.Fatalf("bad -deny pattern %q: %v", p, err)
			}
			cfg.Deny = append(cfg.Deny, re)
		}
	}

	log.SetFlags(log.Ltime)
	log.SetPrefix("")

	d, err := daemon.New(cfg)
	if err != nil {
		log.Fatalf("ariadned: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Printf("ariadned: shutting down")
		cancel()
	}()

	if err := d.Serve(ctx); err != nil {
		log.Fatalf("ariadned: %v", err)
	}
}
