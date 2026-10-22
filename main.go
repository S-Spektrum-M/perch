// Command pircd serves a small web UI for remote-controlling pi coding agent
// sessions. Each attached session runs as a `pi --mode rpc` child process;
// pages are server-rendered and live updates stream over Server-Sent Events.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	home, _ := os.UserHomeDir()

	defaultSessions := os.Getenv("PI_CODING_AGENT_SESSION_DIR")
	if defaultSessions == "" {
		defaultSessions = filepath.Join(home, ".pi", "agent", "sessions")
	}

	var (
		addr        = flag.String("addr", "", "listen address(es), comma separated (default: tailscale0 IPv4 + 127.0.0.1, port 7337)")
		ntfyTopic   = flag.String("ntfy-topic", os.Getenv("PIRCD_NTFY_TOPIC"), "full ntfy topic URL for done notifications (disabled when empty)")
		ntfyToken   = flag.String("ntfy-token", os.Getenv("PIRCD_NTFY_TOKEN"), "ntfy bearer token (prefer PIRCD_NTFY_TOKEN)")
		publicURL   = flag.String("public-url", os.Getenv("PIRCD_PUBLIC_URL"), "external pircd URL for notification click links")
		piBin       = flag.String("pi", "pi", "path to the pi executable")
		piArgs      = flag.String("pi-args", "", "extra arguments passed to every pi process (space separated, e.g. \"--approve\")")
		sessionsDir = flag.String("sessions", defaultSessions, "pi session directory")
		root        = flag.String("root", home, "root directory for the new-session directory picker")
		idle        = flag.Duration("idle", 30*time.Minute, "stop idle pi processes after this long with no viewers")
		hideStatus  = flag.String("hide-status", "pi-vimmode", "comma-separated extension status keys to hide (TUI-only indicators)")
		uploads     = flag.String("uploads", defaultUploadsDir(), "directory for files attached to prompts (one subdirectory per session)")
		retention   = flag.Duration("upload-retention", 7*24*time.Hour, "delete a session's uploads after it has been unused this long (0 keeps them forever)")
		waitTS      = flag.Duration("wait-tailscale", 0, "without -addr: wait this long for tailscale0 to come up, then exit with an error instead of falling back to loopback (for boot-time services)")
	)
	flag.Parse()

	args := strings.Fields(*piArgs)
	if filepath.Clean(*sessionsDir) != filepath.Clean(defaultSessions) {
		args = append(args, "--session-dir", *sessionsDir)
	}
	hidden := map[string]bool{}
	for _, k := range strings.Split(*hideStatus, ",") {
		if k = strings.TrimSpace(k); k != "" {
			hidden[k] = true
		}
	}
	notifier, err := newNotifier(*ntfyTopic, *ntfyToken, *publicURL)
	if err != nil {
		log.Fatal(err)
	}
	mgr := NewManager(*piBin, args, *idle, hidden)
	mgr.notifier = notifier
	srv := &Server{
		store:   NewStore(*sessionsDir),
		mgr:     mgr,
		root:    filepath.Clean(*root),
		uploads: *uploads,
		tmpl:    loadTemplates(),
	}
	mgr.render = srv.tmpl

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go srv.cleanUploadsLoop(ctx, *retention)

	addrs, err := listenAddrs(ctx, *addr, *waitTS)
	if err != nil {
		log.Fatal(err)
	}
	httpSrv := &http.Server{
		Handler: srv.routes(),
		// cancels open event streams on shutdown
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	for _, a := range addrs {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			log.Fatalf("listen %s: %v", a, err)
		}
		log.Printf("pircd listening on http://%s", ln.Addr())
		go func() {
			if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatal(err)
			}
		}()
	}

	<-ctx.Done()
	log.Printf("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	mgr.CloseAll()
}

// listenAddrs resolves the -addr flag. With no flag, pircd binds to the
// tailscale interface (if present) and loopback only, never to every
// interface: anyone who can reach this server can run commands as you.
//
// With wait > 0 it polls for tailscale0 (at boot pircd may start before
// tailscaled) and fails if it never appears, so a supervisor retries rather
// than leaving pircd silently unreachable.
func listenAddrs(ctx context.Context, flagVal string, wait time.Duration) ([]string, error) {
	if flagVal != "" {
		return strings.Split(flagVal, ","), nil
	}
	const port = "7337"
	deadline := time.Now().Add(wait)
	ts := tailscaleIPs()
	for len(ts) == 0 && wait > 0 && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		ts = tailscaleIPs()
	}
	if len(ts) == 0 {
		if wait > 0 {
			return nil, fmt.Errorf("tailscale0 has no IPv4 address after %s", wait)
		}
		log.Printf("warning: no tailscale0 interface found; listening on loopback only (use -addr)")
	}
	addrs := []string{"127.0.0.1:" + port}
	for _, ip := range ts {
		addrs = append(addrs, net.JoinHostPort(ip, port))
	}
	return addrs, nil
}

func tailscaleIPs() []string {
	iface, err := net.InterfaceByName("tailscale0")
	if err != nil {
		return nil
	}
	ips, err := iface.Addrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, ip := range ips {
		if n, ok := ip.(*net.IPNet); ok && n.IP.To4() != nil {
			out = append(out, n.IP.String())
		}
	}
	return out
}
