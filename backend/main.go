package main

import (
	"context"
	_ "embed"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"rykvo.local/auth/internal/hardware"
)

//go:embed schema.sql
var schema string

func main() {
	migrate := flag.Bool("migrate", false, "Apply database schema without changing credentials")
	initialize := flag.Bool("init", false, "Create tables and the initial admin; read password from stdin")
	initializeVisibility := flag.Bool("init-visibility", false, "Initialize independent display password from stdin")
	card := flag.String("hardware-card", "", "Internal isolated PC/SC reader")
	esim := flag.Bool("hardware-esim", false, "Internal isolated eUICC session")
	wifiCheck := flag.Bool("hardware-wifi-check", false, "Read Wi-Fi calling SIM prerequisites from stdin")
	wifiRun := flag.Bool("hardware-wifi-run", false, "Bounded Wi-Fi calling maintenance test")
	wifiWorker := flag.Bool("hardware-wifi-worker", false, "Private socket-activated VoWiFi session")
	flag.Parse()
	if *wifiWorker {
		if hardware.WiFiWorker() != nil {
			os.Exit(1)
		}
		return
	}
	if *wifiRun {
		if err := hardware.WiFiSIMRun(); err != nil {
			log.Print(err)
			os.Exit(1)
		}
		return
	}
	if *wifiCheck {
		if err := hardware.WiFiSIMCheck(); err != nil {
			log.Print(err)
			os.Exit(1)
		}
		return
	}
	if *esim {
		if hardware.ESIMHelper() != nil {
			os.Exit(1)
		}
		return
	}
	helper := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "hardware-card" {
			helper = true
		}
	})
	if helper {
		if err := hardware.CardHelper(*card); err != nil {
			os.Exit(1)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal("Invalid database configuration")
	}
	cfg.MaxConns, cfg.MinConns = 4, 1
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		log.Fatal("Database unavailable")
	}
	defer pool.Close()
	if err = pool.Ping(ctx); err != nil {
		log.Fatal("Database unavailable")
	}
	if *initialize || *initializeVisibility || *migrate {
		if _, err = pool.Exec(ctx, schema); err != nil {
			log.Fatal("Schema initialization failed")
		}
		if *migrate {
			return
		}
		secret, err := io.ReadAll(io.LimitReader(os.Stdin, 129))
		if err != nil || len(secret) < 8 || len(secret) > 128 {
			log.Fatal("Initial password must be 8–128 bytes")
		}
		salt, hash, err := newPassword(string(secret))
		clear(secret)
		if err != nil {
			log.Fatal("Password initialization failed")
		}
		query := `INSERT INTO administrators(username,password_hash,password_salt,password_iterations) SELECT 'admin',$1,$2,$3 WHERE NOT EXISTS(SELECT 1 FROM administrators) ON CONFLICT(username) DO NOTHING`
		if *initializeVisibility {
			query = `INSERT INTO visibility_security(password_hash,password_salt,password_iterations) VALUES($1,$2,$3) ON CONFLICT(singleton) DO NOTHING`
		}
		result, err := pool.Exec(ctx, query, hash, salt, passwordIterations)
		if err != nil {
			log.Fatal("Account initialization failed")
		}
		log.Printf("Initialized; created records: %d", result.RowsAffected())
		return
	}
	origin := strings.TrimRight(os.Getenv("PUBLIC_ORIGIN"), "/")
	secure := false
	if origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
			log.Fatal("Invalid public origin")
		}
		secure = parsed.Scheme == "https"
	}
	api := &server{db: pool, origin: origin, secure: secure, limits: make(map[string]attempts), slots: make(chan struct{}, 2)}
	api.sipNetwork = &sipNetworkManager{call: sipNetworkCall}
	api.webRoot = os.Getenv("WEB_ROOT")
	if info, err := os.Stat(api.webRoot); err != nil || !info.IsDir() {
		log.Fatal("Invalid web root")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:8080")
	if err != nil {
		log.Fatal("HTTP address unavailable")
	}
	defer listener.Close()
	moduleContext, stopModules := context.WithCancel(ctx)
	system := hardware.NewSystem()
	var wifi wifiSource = system
	switch os.Getenv("RYKVO_WIFI_ENGINE") {
	case "", "legacy":
	case "vocat":
		wifi = &hardware.VocatWorkerClient{}
	default:
		log.Fatal("Invalid Wi-Fi engine selection")
	}
	api.modules = newModuleManagerWithWiFi(pool, system, wifi)
	if client, ok := wifi.(*hardware.VocatWorkerClient); ok {
		client.OnSMS = api.modules.receiveSMS
	}
	sipContext, stopSIP := context.WithCancel(ctx)
	api.sipGateway = newSIPGateway(api)
	go api.sipGateway.run(sipContext)
	defer func() { stopSIP(); <-api.sipGateway.done }()
	messageContext, stopMessages := context.WithCancel(ctx)
	messageDone := make(chan struct{})
	go func() { defer close(messageDone); api.modules.runMessages(messageContext) }()
	defer func() { stopMessages(); <-messageDone }()
	go api.modules.run(moduleContext)
	defer func() { stopModules(); <-api.modules.done }()
	if dir := os.Getenv("TUNNEL_DIR"); dir != "" {
		api.tunnels, err = newTunnelManager(ctx, pool, dir, os.Getenv("TUNNEL_BIN"))
		if err != nil {
			log.Fatal("Tunnel initialization failed")
		}
		defer api.tunnels.workers.Wait()
	}
	httpServer := &http.Server{Addr: "127.0.0.1:8080", Handler: api, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 8192}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	}()
	log.Print("Authentication service listening on 127.0.0.1:8080")
	if err = httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatal("HTTP server stopped unexpectedly")
	}
}
